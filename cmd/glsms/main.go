// Command glsms is a CLI and REST server for reading and sending SMS through
// a GL.iNet router (firmware 4.x, e.g. GL-X3000 / Spitz AX).
//
// Usage:
//
//	glsms [global flags] <command> [args]
//
// Commands:
//
//	status                 show modem/SIM status and new message count
//	list                   list stored SMS messages
//	send  -to N -body T     send an SMS
//	read  -name NAME        mark a message as read
//	unread -name NAME       mark a message as unread
//	delete -name NAME       delete the message with that storage name
//	tui                     interactive terminal UI
//	serve -addr :8080       run the REST API server
//
// Connection settings come from flags or environment:
//
//	GL_HOST (default 192.168.8.1), GL_USER (default root), GL_PASS (required)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"

	"github.com/CaseyBlackburn/glsms/glsms"
	"github.com/CaseyBlackburn/glsms/tui"
)

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// Must exceed a worst-case send (modem wait + margin); the send
		// handler caps its own context, so this is just a hard backstop.
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("glsms", flag.ContinueOnError)
	host := global.String("host", env("GL_HOST", "192.168.8.1"), "router host or URL")
	user := global.String("user", env("GL_USER", "root"), "router username")
	pass := global.String("pass", os.Getenv("GL_PASS"), "router password (or set GL_PASS)")
	jsonOut := global.Bool("json", false, "output JSON")
	global.Usage = usage(global)
	if err := global.Parse(args); err != nil {
		return err
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return fmt.Errorf("no command given")
	}
	cmd, cmdArgs := rest[0], rest[1:]

	// help never needs a connection.
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		global.Usage()
		return nil
	}
	if *pass == "" {
		return fmt.Errorf("router password required (-pass or GL_PASS)")
	}

	client := glsms.New(*host, *user, *pass)
	sms := glsms.NewSMS(client)

	switch cmd {
	case "status":
		return cmdStatus(sms, *jsonOut)
	case "list", "ls":
		return cmdList(sms, cmdArgs, *jsonOut)
	case "send":
		return cmdSend(sms, cmdArgs)
	case "read":
		return cmdRead(sms, cmdArgs)
	case "unread":
		return cmdUnread(sms, cmdArgs)
	case "delete", "rm":
		return cmdDelete(sms, cmdArgs, *jsonOut)
	case "serve":
		return cmdServe(sms, cmdArgs)
	case "tui", "ui":
		if err := tui.Run(sms); err != nil {
			return fmt.Errorf("tui: %w (a real interactive terminal is required)", err)
		}
		return nil
	case "help", "-h", "--help":
		global.Usage()
		return nil
	default:
		global.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// ctx is the deadline for read/status/read-mark/delete commands. Delete polls
// the modem for up to ~36s after issuing the removal, so keep this generous.
func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 120*time.Second)
}

func cmdStatus(sms *glsms.SMS, jsonOut bool) error {
	c, cancel := ctx()
	defer cancel()
	st, err := sms.Status(c)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(st)
	}
	fmt.Printf("Number:     %s\n", st.SIM.PhoneNumber)
	fmt.Printf("Carrier:    %s\n", st.SIM.Carrier)
	fmt.Printf("Network:    %s (signal %d/5)\n", st.SIM.NetworkType, st.SIM.SignalBars)
	fmt.Printf("Bus:        %s\n", st.Bus)
	fmt.Printf("New SMS:    %d\n", st.NewSMSCount)
	return nil
}

func cmdList(sms *glsms.SMS, args []string, jsonOut bool) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	dir := fs.String("dir", "", "filter by direction: sent | received")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir != "" && *dir != string(glsms.Sent) && *dir != string(glsms.Received) {
		return fmt.Errorf("-dir must be 'sent' or 'received'")
	}
	c, cancel := ctx()
	defer cancel()
	all, err := sms.List(c)
	if err != nil {
		return err
	}
	msgs := all
	if *dir != "" {
		msgs = msgs[:0:0]
		for _, m := range all {
			if string(m.Direction) == *dir {
				msgs = append(msgs, m)
			}
		}
	}
	if jsonOut {
		return printJSON(msgs)
	}
	if len(msgs) == 0 {
		fmt.Println("(no messages)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDIR\tPARTY\tDATE\tBODY")
	for _, m := range msgs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			m.Name, dirArrow(m.Direction), m.PhoneNumber, m.DateRaw, truncate(m.Body, 50))
	}
	return tw.Flush()
}

func dirArrow(d glsms.Direction) string {
	switch d {
	case glsms.Received:
		return "in <-"
	case glsms.Sent:
		return "out ->"
	default:
		return "?"
	}
}

func cmdSend(sms *glsms.SMS, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	to := fs.String("to", "", "recipient phone number (with country code)")
	body := fs.String("body", "", "message text")
	timeout := fs.Int("timeout", 60, "seconds the router waits for modem confirmation (0 = don't wait)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" || *body == "" {
		return fmt.Errorf("send requires -to and -body")
	}
	// Outlast the router-side modem wait (*timeout) with margin.
	c, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout+120)*time.Second)
	defer cancel()
	if err := sms.Send(c, *to, *body, *timeout); err != nil {
		return err
	}
	fmt.Printf("sent to %s\n", *to)
	return nil
}

func cmdRead(sms *glsms.SMS, args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	name := fs.String("name", "", "storage name of the message to mark read (from 'list')")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("read requires -name")
	}
	c, cancel := ctx()
	defer cancel()
	if err := sms.MarkRead(c, *name); err != nil {
		return err
	}
	fmt.Printf("marked %s read\n", *name)
	return nil
}

func cmdUnread(sms *glsms.SMS, args []string) error {
	fs := flag.NewFlagSet("unread", flag.ContinueOnError)
	name := fs.String("name", "", "storage name of the message to mark unread (from 'list')")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("unread requires -name")
	}
	c, cancel := ctx()
	defer cancel()
	if err := sms.MarkUnread(c, *name); err != nil {
		return err
	}
	fmt.Printf("marked %s unread\n", *name)
	return nil
}

func cmdDelete(sms *glsms.SMS, args []string, jsonOut bool) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	name := fs.String("name", "", "storage name of the message to delete (from 'list')")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("delete requires -name")
	}
	c, cancel := ctx()
	defer cancel()
	remaining, err := sms.Delete(c, *name)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(remaining)
	}
	fmt.Printf("deleted %s (%d message(s) remaining)\n", *name, len(remaining))
	return nil
}

func cmdServe(sms *glsms.SMS, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", env("GLSMS_ADDR", ":8080"), "listen address")
	token := fs.String("token", os.Getenv("GLSMS_TOKEN"), "require this bearer token on /api/ (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h := glsms.Handler(sms, glsms.ServerConfig{AuthToken: *token})
	srv := newServer(*addr, h)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	go func() {
		<-stop
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	authNote := "no auth"
	if *token != "" {
		authNote = "bearer-token auth"
	}
	fmt.Printf("glsms REST API on %s (%s)\n", *addr, authNote)
	if err := srv.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
		return err
	}
	fmt.Println("shutdown")
	return nil
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func usage(fs *flag.FlagSet) func() {
	return func() {
		fmt.Fprint(os.Stderr, `glsms - SMS over a GL.iNet router RPC

Usage:
  glsms [flags] <command> [args]

Commands:
  status                 show modem/SIM status and new-message count
  list                   list stored SMS messages
  send   -to N -body T   send an SMS
  read   -name NAME      mark a message as read
  unread -name NAME      mark a message as unread
  delete -name NAME      delete the message with that storage name
  tui                    interactive terminal UI
  serve  -addr :8080     run the REST API server

Global flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, "\nEnv: GL_HOST, GL_USER, GL_PASS, GLSMS_ADDR, GLSMS_TOKEN\n")
	}
}
