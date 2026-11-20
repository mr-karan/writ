// Package notify delivers asynchronous notifications for writ serve events
// to Slack incoming webhooks and generic JSON webhooks with optional
// HMAC-SHA256 signing. It imports only the standard library.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// FormatNotification is the JSON format of a webhook notification payload.
const FormatNotification = "writ.notification/v0"

// Kind is the delivery format of a notifier.
type Kind string

const (
	KindSlack   Kind = "slack"
	KindWebhook Kind = "webhook"
)

// ParseKind parses a notifier kind from config.
func ParseKind(s string) (Kind, error) {
	switch Kind(s) {
	case KindSlack, KindWebhook:
		return Kind(s), nil
	default:
		return "", fmt.Errorf("notifier kind %q is not slack or webhook", s)
	}
}

// EventKind names one kind of event a notifier can subscribe to.
type EventKind string

const (
	EventDrift          EventKind = "drift"
	EventPlanFailed     EventKind = "plan-failed"
	EventApplyFailed    EventKind = "apply-failed"
	EventApplied        EventKind = "applied"
	EventOutsideWrite   EventKind = "outside-write"
	EventApprovalNeeded EventKind = "approval-needed"
	EventTest           EventKind = "test"
)

// Events lists the configurable notification event kinds.
var Events = []EventKind{
	EventDrift,
	EventPlanFailed,
	EventApplyFailed,
	EventApplied,
	EventOutsideWrite,
	EventApprovalNeeded,
}

// ParseEvent parses a configurable event kind.
func ParseEvent(s string) (EventKind, error) {
	if slices.Contains(Events, EventKind(s)) {
		return EventKind(s), nil
	}
	names := make([]string, len(Events))
	for i, ev := range Events {
		names[i] = string(ev)
	}
	return "", fmt.Errorf("notification event %q is not one of %s", s, strings.Join(names, ", "))
}

// DriftMode controls whether drift notifications fire only on state changes
// or on every drifted check.
type DriftMode string

const (
	DriftOnChange   DriftMode = "on-change"
	DriftEveryCheck DriftMode = "every-check"
)

// ParseDriftMode parses drift_mode from config.
func ParseDriftMode(s string) (DriftMode, error) {
	switch DriftMode(s) {
	case DriftOnChange, DriftEveryCheck:
		return DriftMode(s), nil
	default:
		return "", fmt.Errorf("drift_mode %q is not on-change or every-check", s)
	}
}

// Bounds and defaults for outbound notification delivery.
const (
	DefaultQueueSize = 128
	DefaultTimeout   = 10 * time.Second
	MaxBodyBytes     = 16 * 1024
	maxResponseDrain = 4096
)

// DefaultBackoff spaces retries across a little over four minutes for
// transient 5xx and timeout failures.
var DefaultBackoff = []time.Duration{
	time.Second,
	4 * time.Second,
	16 * time.Second,
	time.Minute,
	3 * time.Minute,
}

// Actor describes who or what started the action behind a notification.
type Actor struct {
	Kind    string `json:"kind"`
	Display string `json:"display"`
	Person  string `json:"person,omitempty"`
}

// Counts holds numeric counts relevant to an event.
type Counts struct {
	Changes   int   `json:"changes,omitempty"`
	Added     int   `json:"added,omitempty"`
	Changed   int   `json:"changed,omitempty"`
	Destroyed int   `json:"destroyed,omitempty"`
	Imported  int   `json:"imported,omitempty"`
	Outside   int   `json:"outside,omitempty"`
	Code      int   `json:"code,omitempty"`
	Need      int   `json:"need,omitempty"`
	Serial    int64 `json:"serial,omitempty"`
}

// Event is one notification emitted by writ serve. It never holds secret
// values, plan bodies, or raw diagnostics.
type Event struct {
	Format    string    `json:"format"`
	ID        string    `json:"id"`
	Event     EventKind `json:"event"`
	At        time.Time `json:"at"`
	Server    string    `json:"server"`
	Stack     string    `json:"stack,omitempty"`
	Env       string    `json:"env,omitempty"`
	Status    string    `json:"status"`
	Summary   string    `json:"summary"`
	StartedBy Actor     `json:"started_by"`
	Plan      string    `json:"plan,omitempty"`
	Run       string    `json:"run,omitempty"`
	Counts    *Counts   `json:"counts,omitempty"`
	URL       string    `json:"url,omitempty"`
	// DriftStateChanged reports whether a drift check changed state
	// (clean->drifted or drifted->clean). It is omitted from the JSON wire
	// format and used only for per-target drift_mode filtering.
	DriftStateChanged bool `json:"-"`
}

// Target is one resolved notification destination. Its URL and Secret stay
// in memory and are never logged or returned in errors.
type Target struct {
	Name      string
	Kind      Kind
	URL       string
	Secret    []byte
	Events    []EventKind
	DriftMode DriftMode
	// Match filters by environment and stack when non-nil.
	Match func(env, stack string) bool
}

// Result reports the final outcome of delivering one event to one target,
// or of Dropped events that found the target's queue full.
type Result struct {
	Target   string
	Event    EventKind
	OK       bool
	Err      string
	Attempts int
	Dropped  int64
	At       time.Time
}

// Options configures New.
type Options struct {
	Targets   []Target
	QueueSize int
	Timeout   time.Duration
	Backoff   []time.Duration
	Version   string
	Log       *slog.Logger
	Now       func() time.Time
	OnResult  func(Result)
}

// CheckURL validates a webhook URL: https://, or http:// on a loopback
// address for tests, with a host and no embedded credentials or fragment.
// Errors never include the URL's path, query, or userinfo.
func CheckURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return errors.New("webhook url is empty")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("webhook url is malformed")
	}
	if u.Host == "" {
		return errors.New("webhook url has no host")
	}
	if u.User != nil {
		return errors.New("webhook url must not contain userinfo")
	}
	if u.Fragment != "" {
		return errors.New("webhook url must not contain a fragment")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Host) {
			return nil
		}
		return errors.New("webhook url must use https (http is allowed only on loopback)")
	default:
		return fmt.Errorf("webhook url scheme %q is not https", u.Scheme)
	}
}

func isLoopbackHost(hostPort string) bool {
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Sign computes the X-Writ-Signature-256 header value for timestamp and body:
// "sha256=" followed by lowercase hex HMAC-SHA256 over "<timestamp>.<body>".
func Sign(secret []byte, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks header against the HMAC-SHA256 signature of timestamp and body
// in constant time.
func Verify(secret []byte, timestamp int64, body []byte, header string) bool {
	gotHex, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(gotHex)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// FormatWebhook encodes ev as writ.notification/v0 JSON.
func FormatWebhook(ev Event) ([]byte, error) {
	ev.Format = FormatNotification
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	} else {
		ev.At = ev.At.UTC()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBodyBytes {
		return nil, fmt.Errorf("notification body exceeds %d bytes", MaxBodyBytes)
	}
	return b, nil
}

// FormatSlack encodes ev as a Slack incoming webhook JSON payload, escaping
// Slack mrkdwn control characters in untrusted text fields.
func FormatSlack(ev Event) ([]byte, error) {
	ev.Stack, ev.Env, ev.Summary, ev.StartedBy.Display = oneLine(ev.Stack), oneLine(ev.Env), oneLine(ev.Summary), oneLine(ev.StartedBy.Display)
	target := ev.Stack
	if ev.Env != "" {
		if target != "" {
			target += "@" + ev.Env
		} else {
			target = ev.Env
		}
	}
	var fallback string
	if target != "" {
		fallback = fmt.Sprintf("[%s] %s: %s", escapeSlack(string(ev.Event)), escapeSlack(target), escapeSlack(ev.Summary))
	} else {
		fallback = fmt.Sprintf("[%s] %s", escapeSlack(string(ev.Event)), escapeSlack(ev.Summary))
	}
	var line strings.Builder
	fmt.Fprintf(&line, "*[%s]*", escapeSlack(string(ev.Event)))
	if target != "" {
		fmt.Fprintf(&line, " `%s`", escapeSlack(strings.ReplaceAll(target, "`", "'")))
	}
	if ev.Summary != "" {
		fmt.Fprintf(&line, ": %s", escapeSlack(ev.Summary))
	}
	var meta []string
	if ev.StartedBy.Display != "" {
		// In a code span, where neither Slack nor Mattermost makes a link
		// or a mention, so a display name a person chose can't add either.
		meta = append(meta, "Started by: `"+escapeSlack(strings.ReplaceAll(ev.StartedBy.Display, "`", "'"))+"`")
	}
	if ev.URL != "" {
		meta = append(meta, fmt.Sprintf("<%s|View in Writ>", escapeSlack(ev.URL)))
	}
	if len(meta) > 0 {
		line.WriteString("\n")
		line.WriteString(strings.Join(meta, " · "))
	}
	payload := struct {
		Text   string `json:"text"`
		Blocks []struct {
			Type string `json:"type"`
			Text struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}{
		Text: fallback,
	}
	blk := struct {
		Type string `json:"type"`
		Text struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"text"`
	}{Type: "section"}
	blk.Text.Type = "mrkdwn"
	blk.Text.Text = line.String()
	payload.Blocks = append(payload.Blocks, blk)
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBodyBytes {
		return nil, fmt.Errorf("slack notification body exceeds %d bytes", MaxBodyBytes)
	}
	return b, nil
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeSlack(s string) string {
	return slackEscaper.Replace(s)
}

var errRedirectRefused = errors.New("redirect refused")

func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: min(5*time.Second, timeout)}).DialContext,
		TLSHandshakeTimeout:   min(5*time.Second, timeout),
		ResponseHeaderTimeout: timeout,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errRedirectRefused
		},
	}
}

// Dispatcher queues and delivers notifications asynchronously with one worker
// per configured target.
type Dispatcher struct {
	workers  []*worker
	client   *http.Client
	backoff  []time.Duration
	version  string
	log      *slog.Logger
	now      func() time.Time
	onResult func(Result)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type worker struct {
	target Target
	queue  chan Event
	// dropped counts events Emit found no room for. The worker reports
	// them, so Emit never waits on OnResult.
	dropped atomic.Int64
}

// New starts a Dispatcher for opts.Targets. Call Close to stop its workers.
func New(opts Options) *Dispatcher {
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Backoff == nil {
		opts.Backoff = DefaultBackoff
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		client:   newHTTPClient(opts.Timeout),
		backoff:  slices.Clone(opts.Backoff),
		version:  opts.Version,
		log:      opts.Log,
		now:      opts.Now,
		onResult: opts.OnResult,
		ctx:      ctx,
		cancel:   cancel,
	}
	for _, t := range opts.Targets {
		if t.DriftMode == "" {
			t.DriftMode = DriftOnChange
		}
		w := &worker{target: t, queue: make(chan Event, opts.QueueSize)}
		d.workers = append(d.workers, w)
		d.wg.Go(func() { d.runWorker(w) })
	}
	return d
}

// Close cancels pending deliveries and waits for all worker goroutines to exit.
func (d *Dispatcher) Close() {
	if d == nil {
		return
	}
	d.cancel()
	d.wg.Wait()
	d.client.CloseIdleConnections()
}

// Emit queues ev for every matching target without blocking. If a target's
// queue is full, the event is dropped for that target and logged, and the
// target's worker reports the drop through OnResult after its current
// delivery.
func (d *Dispatcher) Emit(ev Event) {
	if d == nil || len(d.workers) == 0 {
		return
	}
	if ev.At.IsZero() {
		ev.At = d.now().UTC()
	}
	for _, w := range d.workers {
		if !w.matches(ev) {
			continue
		}
		select {
		case w.queue <- ev:
		default:
			w.dropped.Add(1)
			d.log.Warn("notification queue full, dropping event", "notifier", w.target.Name, "event", ev.Event, "stack", ev.Stack, "env", ev.Env)
		}
	}
}

func (w *worker) matches(ev Event) bool {
	if !slices.Contains(w.target.Events, ev.Event) {
		return false
	}
	if ev.Event == EventDrift && w.target.DriftMode != DriftEveryCheck && !ev.DriftStateChanged {
		return false
	}
	if w.target.Match != nil && !w.target.Match(ev.Env, ev.Stack) {
		return false
	}
	return true
}

func (d *Dispatcher) runWorker(w *worker) {
	for {
		select {
		case <-d.ctx.Done():
			return
		case ev := <-w.queue:
			d.deliver(w.target, ev)
			if n := w.dropped.Swap(0); n > 0 {
				d.report(Result{Target: w.target.Name, Event: ev.Event, Err: fmt.Sprintf("queue full: dropped %d events", n), Dropped: n, At: d.now().UTC()})
			}
		}
	}
}

func (d *Dispatcher) deliver(target Target, ev Event) {
	var lastErr string
	attempts := 0
	for attempt := 0; attempt <= len(d.backoff); attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(d.backoff[attempt-1])
			select {
			case <-d.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		attempts++
		retryable, errMsg := sendOnce(d.ctx, d.client, target, ev, d.now(), d.version)
		if errMsg == "" {
			d.log.Debug("notification delivered", "notifier", target.Name, "event", ev.Event, "stack", ev.Stack, "env", ev.Env, "attempts", attempts)
			d.report(Result{
				Target:   target.Name,
				Event:    ev.Event,
				OK:       true,
				Attempts: attempts,
				At:       d.now().UTC(),
			})
			return
		}
		lastErr = errMsg
		if !retryable || d.ctx.Err() != nil {
			break
		}
	}
	if d.ctx.Err() != nil {
		return
	}
	d.log.Warn("notification delivery failed", "notifier", target.Name, "event", ev.Event, "stack", ev.Stack, "env", ev.Env, "attempts", attempts, "err", lastErr)
	d.report(Result{
		Target:   target.Name,
		Event:    ev.Event,
		OK:       false,
		Err:      lastErr,
		Attempts: attempts,
		At:       d.now().UTC(),
	})
}

func (d *Dispatcher) report(r Result) {
	if d.onResult != nil {
		d.onResult(r)
	}
}

// SendTest sends one synchronous test notification to target with no retries.
func SendTest(ctx context.Context, target Target, ev Event, timeout time.Duration) error {
	client := newHTTPClient(timeout)
	defer client.CloseIdleConnections()
	_, errMsg := sendOnce(ctx, client, target, ev, time.Now().UTC(), "")
	if errMsg != "" {
		return errors.New(errMsg)
	}
	return nil
}

func sendOnce(ctx context.Context, client *http.Client, target Target, ev Event, now time.Time, version string) (retryable bool, errMsg string) {
	if err := CheckURL(target.URL); err != nil {
		return false, err.Error()
	}
	var body []byte
	var err error
	switch target.Kind {
	case KindSlack:
		body, err = FormatSlack(ev)
	case KindWebhook:
		body, err = FormatWebhook(ev)
	default:
		return false, "unknown notifier kind"
	}
	if err != nil {
		return false, err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return false, "invalid request"
	}
	req.Header.Set("Content-Type", "application/json")
	ua := "writ"
	if version != "" {
		ua = "writ/" + version
	}
	req.Header.Set("User-Agent", ua)
	if target.Kind == KindWebhook {
		ts := now.UTC().Unix()
		req.Header.Set("X-Writ-Event", string(ev.Event))
		if ev.ID != "" {
			req.Header.Set("X-Writ-Delivery", ev.ID)
		}
		req.Header.Set("X-Writ-Timestamp", strconv.FormatInt(ts, 10))
		if len(target.Secret) > 0 {
			req.Header.Set("X-Writ-Signature-256", Sign(target.Secret, ts, body))
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return classifyClientErr(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseDrain))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, ""
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return false, "redirect refused"
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, fmt.Sprintf("http status %d", resp.StatusCode)
	default:
		return false, fmt.Sprintf("http status %d", resp.StatusCode)
	}
}

// classifyClientErr turns an http.Client error into a short, sanitized error
// message that never includes the target URL, query parameters, or secrets.
func classifyClientErr(err error) (retryable bool, msg string) {
	if errors.Is(err, errRedirectRefused) {
		return false, "redirect refused"
	}
	if errors.Is(err, context.Canceled) {
		return false, "request canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true, "request timed out"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true, "request timed out"
	}
	return true, "connection failed"
}

// oneLine replaces every control character, and the line and paragraph
// separators, with a space, so a field can't start a line of its own in
// a message.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
}
