package glsms

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// dateLayout matches the router's SMS timestamp, e.g. "26-05-14 21:44:42"
// (YY-MM-DD HH:MM:SS).
const dateLayout = "06-01-02 15:04:05"

// Direction indicates whether a message was sent or received.
type Direction string

const (
	// Received: an inbound message; PhoneNumber is the sender.
	Received Direction = "received"
	// Sent: an outbound message; PhoneNumber is the recipient.
	Sent Direction = "sent"
	// UnknownDirection: could not be determined from the router data.
	UnknownDirection Direction = "unknown"
)

// Message is a single SMS as reported by the router.
type Message struct {
	// Name is the router's opaque storage key for the message
	// (e.g. "GMS_GKdodB" or "GMS1.bcLoPH"). It is the identifier used by
	// Delete and is also the primary signal for Direction.
	Name string `json:"name"`
	// Direction is sent or received (best-effort; see DirectionOf).
	Direction Direction `json:"direction"`
	// PhoneNumber is the other party: the recipient for sent messages,
	// the sender for received messages.
	PhoneNumber string `json:"phone_number"`
	// Sender is the local SIM number, when the router reports it.
	Sender string `json:"sender"`
	// Body is the message text.
	Body string `json:"body"`
	// Date is the router-reported timestamp, parsed in local time.
	// Zero if the router value could not be parsed (see DateRaw).
	Date time.Time `json:"date"`
	// DateRaw is the unparsed timestamp string from the router.
	DateRaw string `json:"date_raw"`
	// Status is the router's raw GSM status code: 0 = received unread,
	// 1 = received read, 2 = sent (see Status* constants).
	Status int `json:"status"`
	// Type is the router's raw "type" field, present only on received
	// messages (value 0); nil for sent messages. Used by DirectionOf.
	Type *int `json:"type,omitempty"`
	// Bus is the modem bus this message belongs to.
	Bus string `json:"bus"`
}

// DirectionOf classifies a message as Sent or Received.
//
// The X3000 firmware exposes no explicit flag, so this uses the two
// empirically-consistent signals: received messages are stored on the SIM
// (name like "GMS1.xxxx" — "GMS" + digits + ".") and carry a "type" field;
// messages this device sent are stored in modem memory (name "GMS_xxxx") and
// omit "type".
func DirectionOf(name string, typeField *int) Direction {
	if typeField != nil {
		return Received
	}
	switch {
	case strings.HasPrefix(name, "GMS_"):
		return Sent
	case simStoredName(name):
		return Received
	default:
		return UnknownDirection
	}
}

// simStoredName reports whether name looks like "GMS<digits>." (SIM storage),
// which the firmware uses for received messages.
func simStoredName(name string) bool {
	rest, ok := strings.CutPrefix(name, "GMS")
	if !ok || rest == "" {
		return false
	}
	dot := strings.IndexByte(rest, '.')
	if dot <= 0 {
		return false
	}
	for _, r := range rest[:dot] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// SIM describes the active SIM and signal, from modem status.
type SIM struct {
	PhoneNumber string `json:"phone_number"`
	Carrier     string `json:"carrier"`
	ICCID       string `json:"iccid"`
	IMSI        string `json:"imsi"`
	APN         string `json:"apn"`
	SignalBars  int    `json:"signal_bars"`
	NetworkType string `json:"network_type"`
}

// ModemStatus is a trimmed view of modem.get_status.
type ModemStatus struct {
	Bus         string `json:"bus"`
	NewSMSCount int    `json:"new_sms_count"`
	SIM         SIM    `json:"sim"`
}

// checkGLError inspects a raw router result. The modem service returns
// {"err_msg":...,"err_code":N} on logical failure (even with HTTP 200), and a
// success payload otherwise (commonly a JSON array, or an object without the
// err fields). It returns a non-nil error only for the failure envelope.
func checkGLError(raw json.RawMessage, op string) error {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil // arrays / non-object success payloads
	}
	var env struct {
		ErrMsg  string `json:"err_msg"`
		ErrCode int    `json:"err_code"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil // not the envelope shape; treat as success payload
	}
	if env.ErrCode != 0 || env.ErrMsg != "" {
		return fmt.Errorf("%s: router error %d: %s", op, env.ErrCode, env.ErrMsg)
	}
	return nil
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}

// SMS provides the SMS operations on top of a Client. The active modem bus is
// discovered once from modem status and cached.
type SMS struct {
	c *Client

	mu  sync.Mutex
	bus string
}

// NewSMS returns an SMS helper bound to the given client.
func NewSMS(c *Client) *SMS { return &SMS{c: c} }

// Bus returns the active modem bus, discovering and caching it on first use.
func (s *SMS) Bus(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bus != "" {
		return s.bus, nil
	}
	st, err := s.status(ctx)
	if err != nil {
		return "", err
	}
	if st.Bus == "" {
		return "", fmt.Errorf("no modem found on router")
	}
	s.bus = st.Bus
	return s.bus, nil
}

type rawStatus struct {
	Modems []struct {
		Bus     string `json:"bus"`
		SIMCard struct {
			PhoneNumber string `json:"phone_number"`
			Carrier     string `json:"carrier"`
			ICCID       string `json:"iccid"`
			IMSI        string `json:"imsi"`
			APN         string `json:"apn"`
			Signal      struct {
				Strength    int    `json:"strength"`
				NetworkType string `json:"network_type"`
			} `json:"signal"`
		} `json:"simcard"`
	} `json:"modems"`
	NewSMSCount int `json:"new_sms_count"`
}

func (s *SMS) status(ctx context.Context) (ModemStatus, error) {
	var rs rawStatus
	if err := s.c.Call(ctx, "modem", "get_status", nil, &rs); err != nil {
		return ModemStatus{}, err
	}
	if len(rs.Modems) == 0 {
		return ModemStatus{NewSMSCount: rs.NewSMSCount}, nil
	}
	m := rs.Modems[0]
	return ModemStatus{
		Bus:         m.Bus,
		NewSMSCount: rs.NewSMSCount,
		SIM: SIM{
			PhoneNumber: m.SIMCard.PhoneNumber,
			Carrier:     m.SIMCard.Carrier,
			ICCID:       m.SIMCard.ICCID,
			IMSI:        m.SIMCard.IMSI,
			APN:         m.SIMCard.APN,
			SignalBars:  m.SIMCard.Signal.Strength,
			NetworkType: m.SIMCard.Signal.NetworkType,
		},
	}, nil
}

// Status returns a trimmed modem/SIM status, including the count of new
// (unread) messages.
func (s *SMS) Status(ctx context.Context) (ModemStatus, error) {
	return s.status(ctx)
}

// List returns all SMS messages currently stored on the modem.
func (s *SMS) List(ctx context.Context) ([]Message, error) {
	bus, err := s.Bus(ctx)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := s.c.Call(ctx, "modem", "get_sms_list", map[string]any{"bus": bus}, &raw); err != nil {
		return nil, err
	}
	if err := checkGLError(raw, "list"); err != nil {
		return nil, err
	}
	var res struct {
		List []struct {
			Name        string `json:"name"`
			PhoneNumber string `json:"phone_number"`
			Sender      string `json:"sender"`
			Body        string `json:"body"`
			Date        string `json:"date"`
			Status      int    `json:"status"`
			Type        *int   `json:"type"`
			Bus         string `json:"bus"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("list: decode: %w", err)
	}
	out := make([]Message, 0, len(res.List))
	for _, m := range res.List {
		msg := Message{
			Name:        m.Name,
			Direction:   DirectionOf(m.Name, m.Type),
			PhoneNumber: m.PhoneNumber,
			Sender:      m.Sender,
			Body:        m.Body,
			DateRaw:     m.Date,
			Status:      m.Status,
			Type:        m.Type,
			Bus:         m.Bus,
		}
		if t, perr := time.ParseInLocation(dateLayout, m.Date, time.Local); perr == nil {
			msg.Date = t
		}
		out = append(out, msg)
	}
	return out, nil
}

// Send sends an SMS with the given body to the given phone number. timeout is
// how long (seconds) the router waits for the modem to confirm; 0 returns
// immediately without waiting for delivery.
func (s *SMS) Send(ctx context.Context, to, body string, timeoutSec int) error {
	bus, err := s.Bus(ctx)
	if err != nil {
		return err
	}
	if to == "" || body == "" {
		return fmt.Errorf("send: both recipient and body are required")
	}
	var raw json.RawMessage
	err = s.c.Call(ctx, "modem", "send_sms", map[string]any{
		"bus":          bus,
		"phone_number": to,
		"body":         body,
		"timeout":      timeoutSec,
	}, &raw)
	if err != nil {
		return err
	}
	return checkGLError(raw, "send")
}

// SMS status codes used by the modem service. These are the standard GSM
// +CMGL stat values, confirmed against an X3000 (a status-0 message matches
// modem.get_status new_sms_count exactly):
//
//	0 = received, unread (new)
//	1 = received, read
//	2 = sent
//
// Deletion also uses status 0 as the "removable" flag (set_sms then
// remove_sms); see Delete.
const (
	StatusReceivedUnread = 0
	StatusReceivedRead   = 1
	StatusSent           = 2

	statusToDelete = StatusReceivedUnread // remove_sms only deletes status-0 msgs
)

// setStatus calls modem.set_sms to change a message's status code.
func (s *SMS) setStatus(ctx context.Context, bus, name string, status int) error {
	var raw json.RawMessage
	if err := s.c.Call(ctx, "modem", "set_sms", map[string]any{
		"bus":    bus,
		"name":   name,
		"status": status,
	}, &raw); err != nil {
		return err
	}
	return checkGLError(raw, "set_sms")
}

// MarkRead marks the message with the given Name as read.
func (s *SMS) MarkRead(ctx context.Context, name string) error {
	bus, err := s.Bus(ctx)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("mark read: message name is required")
	}
	return s.setStatus(ctx, bus, name, StatusReceivedRead)
}

// MarkUnread marks a received message with the given Name back to unread.
// (set_sms(status=0) only changes the flag; it does not delete — deletion
// additionally requires remove_sms.)
func (s *SMS) MarkUnread(ctx context.Context, name string) error {
	bus, err := s.Bus(ctx)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("mark unread: message name is required")
	}
	return s.setStatus(ctx, bus, name, StatusReceivedUnread)
}

// Delete removes the message identified by its Name (the storage key from
// List). The modem requires a two-step protocol: the message must first be
// flagged via set_sms(status=0), then removed via remove_sms; the actual
// removal completes a moment later, so this polls briefly to confirm. It
// returns the remaining messages.
func (s *SMS) Delete(ctx context.Context, name string) ([]Message, error) {
	bus, err := s.Bus(ctx)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("delete: message name is required")
	}
	if err := s.setStatus(ctx, bus, name, statusToDelete); err != nil {
		return nil, fmt.Errorf("delete: flag step: %w", err)
	}
	var raw json.RawMessage
	if err := s.c.Call(ctx, "modem", "remove_sms", map[string]any{
		"bus":  bus,
		"name": name,
	}, &raw); err != nil {
		return nil, err
	}
	if err := checkGLError(raw, "delete"); err != nil {
		return nil, err
	}
	// Removal is not instantaneous; confirm by polling the list briefly.
	var last []Message
	for i := 0; i < 12; i++ {
		last, err = s.List(ctx)
		if err != nil {
			return nil, err
		}
		if !containsName(last, name) {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return last, fmt.Errorf("delete: message %q still present after removal request", name)
}

func containsName(ms []Message, name string) bool {
	for _, m := range ms {
		if m.Name == name {
			return true
		}
	}
	return false
}
