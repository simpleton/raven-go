// Package raven implements a client for the Sentry error logging service.
package raven

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	userAgent       = "raven-go/1.0"
	timestampFormat = `"2006-01-02T15:04:05"`
)

var (
	ErrPacketDropped         = errors.New("raven: packet dropped")
	ErrUnableToUnmarshalJSON = errors.New("raven: unable to unmarshal JSON")
	ErrMissingUser           = errors.New("raven: dns missing public key and/or password")
	ErrMissingPrivateKey     = errors.New("raven: dsn missing private key")
	ErrMissingProjectID      = errors.New("raven: dsn missing project id")
)

type Severity string

// http://docs.python.org/2/howto/logging.html#logging-levels
const (
	DEBUG   = Severity("debug")
	INFO    = Severity("info")
	WARNING = Severity("warning")
	ERROR   = Severity("error")
	FATAL   = Severity("fatal")
)

type Timestamp time.Time

func (t Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(time.Time(t).UTC().Format(timestampFormat)), nil
}

func (timestamp *Timestamp) UnmarshalJSON(data []byte) error {
	t, err := time.Parse(timestampFormat, string(data))
	if err != nil {
		return err
	}

	*timestamp = Timestamp(t)
	return nil
}

// An Interface is a Sentry interface that will be serialized as JSON.
// It must implement json.Marshaler or use json struct tags.
type Interface interface {
	// The Sentry class name. Example: sentry.interfaces.Stacktrace
	Class() string
}

type Culpriter interface {
	Culprit() string
}

type Transport interface {
	Send(url, authHeader string, packet *Packet) error
}

type outgoingPacket struct {
	packet *Packet
	ch     chan error
}

type Tag struct {
	Key   string
	Value string
}

type Tags []Tag

func (tag *Tag) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]string{tag.Key, tag.Value})
}

func (t *Tag) UnmarshalJSON(data []byte) error {
	var tag [2]string
	if err := json.Unmarshal(data, &tag); err != nil {
		return err
	}
	*t = Tag{tag[0], tag[1]}
	return nil
}

func (t *Tags) UnmarshalJSON(data []byte) error {
	var tags []Tag

	switch data[0] {
	case '[':
		// Unmarshal into []Tag
		if err := json.Unmarshal(data, &tags); err != nil {
			return err
		}
	case '{':
		// Unmarshal into map[string]string
		tagMap := make(map[string]string)
		if err := json.Unmarshal(data, &tagMap); err != nil {
			return err
		}

		// Convert to []Tag
		for k, v := range tagMap {
			tags = append(tags, Tag{k, v})
		}
	default:
		return ErrUnableToUnmarshalJSON
	}

	*t = tags
	return nil
}

// http://sentry.readthedocs.org/en/latest/developer/client/index.html#building-the-json-packet
type Packet struct {
	// Required
	Message string `json:"message"`

	// Required, set automatically by Client.Send/Report via Packet.Init if blank
	EventID   string    `json:"event_id"`
	Project   string    `json:"project"`
	Timestamp Timestamp `json:"timestamp"`
	Level     Severity  `json:"level"`
	Logger    string    `json:"logger"`

	// Optional
	Platform   string                 `json:"platform,omitempty"`
	Culprit    string                 `json:"culprit,omitempty"`
	ServerName string                 `json:"server_name,omitempty"`
	Release    string                 `json:"release,omitempty"`
	Tags       Tags                   `json:"tags,omitempty"`
	Modules    []map[string]string    `json:"modules,omitempty"`
	Extra      map[string]interface{} `json:"extra,omitempty"`

	Interfaces []Interface `json:"-"`
}

// NewPacket constructs a packet with the specified message and interfaces.
func NewPacket(message string, interfaces ...Interface) *Packet {
	extra := map[string]interface{}{
		"runtime.Version":      runtime.Version(),
		"runtime.NumCPU":       runtime.NumCPU(),
		"runtime.GOMAXPROCS":   runtime.GOMAXPROCS(0), // 0 just returns the current value
		"runtime.NumGoroutine": runtime.NumGoroutine(),
	}
	return &Packet{
		Message:    message,
		Interfaces: interfaces,
		Extra:      extra,
	}
}

// Init initializes required fields in a packet. It is typically called by
// Client.Send/Report automatically.
func (packet *Packet) Init(project string) error {
	if packet.Project == "" {
		packet.Project = project
	}
	if packet.EventID == "" {
		var err error
		packet.EventID, err = uuid()
		if err != nil {
			return err
		}
	}
	if time.Time(packet.Timestamp).IsZero() {
		packet.Timestamp = Timestamp(time.Now())
	}
	if packet.Level == "" {
		packet.Level = ERROR
	}
	if packet.Logger == "" {
		packet.Logger = "root"
	}
	if packet.ServerName == "" {
		packet.ServerName = hostname
	}
	if packet.Platform == "" {
		packet.Platform = "go"
	}

	if packet.Culprit == "" {
		for _, inter := range packet.Interfaces {
			if c, ok := inter.(Culpriter); ok {
				packet.Culprit = c.Culprit()
				if packet.Culprit != "" {
					break
				}
			}
		}
	}

	return nil
}

func (packet *Packet) AddTags(tags map[string]string) {
	for k, v := range tags {
		packet.Tags = append(packet.Tags, Tag{k, v})
	}
}

func uuid() (string, error) {
	id := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, id)
	if err != nil {
		return "", err
	}
	id[6] &= 0x0F // clear version
	id[6] |= 0x40 // set version to 4 (random uuid)
	id[8] &= 0x3F // clear variant
	id[8] |= 0x80 // set to IETF variant
	return hex.EncodeToString(id), nil
}

func (packet *Packet) JSON() []byte {
	packetJSON, _ := json.Marshal(packet)

	interfaces := make(map[string]Interface, len(packet.Interfaces))
	for _, inter := range packet.Interfaces {
		interfaces[inter.Class()] = inter
	}

	if len(interfaces) > 0 {
		interfaceJSON, _ := json.Marshal(interfaces)
		packetJSON[len(packetJSON)-1] = ','
		packetJSON = append(packetJSON, interfaceJSON[1:]...)
	}

	return packetJSON
}

// The maximum number of packets that will be buffered waiting to be delivered.
// Packets will be dropped if the buffer is full. Used by NewClient.
var MaxQueueBuffer = 100

func newClient(tags map[string]string) *Client {
	client := &Client{Transport: &HTTPTransport{}, Tags: tags, queue: make(chan *outgoingPacket, MaxQueueBuffer)}
	go client.worker()
	client.SetDSN(os.Getenv("SENTRY_DSN"))
	return client
}

// New constructs a new Sentry client instance
func New(dsn string) (*Client, error) {
	client := newClient(nil)
	return client, client.SetDSN(dsn)
}

// NewWithTags constructs a new Sentry client instance with default tags.
func NewWithTags(dsn string, tags map[string]string) (*Client, error) {
	client := newClient(tags)
	return client, client.SetDSN(dsn)
}

// NewClient constructs a Sentry client and spawns a background goroutine to
// handle packets sent by Client.Report.
//
// Deprecated: use New and NewWithTags instead
func NewClient(dsn string, tags map[string]string) (*Client, error) {
	client := newClient(tags)
	return client, client.SetDSN(dsn)
}

// Client encapsulates a connection to a Sentry server. It must be initialized
// by calling NewClient. Modification of fields concurrently with Send or after
// calling Report for the first time is not thread-safe.
type Client struct {
	Tags map[string]string

	Transport Transport

	// DropHandler is called when a packet is dropped because the buffer is full.
	DropHandler func(*Packet)

	mu         sync.RWMutex
	url        string
	projectID  string
	authHeader string
	release    string
	queue      chan *outgoingPacket
}

// Initialize a default *Client instance
var DefaultClient = newClient(nil)

// SetDSN updates a client with a new DSN. It safe to call after and
// concurrently with calls to Report and Send.
func (client *Client) SetDSN(dsn string) error {
	if dsn == "" {
		return nil
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	uri, err := url.Parse(dsn)
	if err != nil {
		return err
	}

	if uri.User == nil {
		return ErrMissingUser
	}
	publicKey := uri.User.Username()
	secretKey, ok := uri.User.Password()
	if !ok {
		return ErrMissingPrivateKey
	}
	uri.User = nil

	if idx := strings.LastIndex(uri.Path, "/"); idx != -1 {
		client.projectID = uri.Path[idx+1:]
		uri.Path = uri.Path[:idx+1] + "api/" + client.projectID + "/store/"
	}
	if client.projectID == "" {
		return ErrMissingProjectID
	}

	client.url = uri.String()

	client.authHeader = fmt.Sprintf("Sentry sentry_version=4, sentry_key=%s, sentry_secret=%s", publicKey, secretKey)

	return nil
}

// Sets the DSN for the default *Client instance
func SetDSN(dsn string) error { return DefaultClient.SetDSN(dsn) }

// SetRelease sets the "release" tag.
func (client *Client) SetRelease(release string) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.release = release
}

// SetRelease sets the "release" tag on the default *Client
func SetRelease(release string) { DefaultClient.SetRelease(release) }

func (client *Client) worker() {
	for outgoingPacket := range client.queue {

		client.mu.RLock()
		url, authHeader := client.url, client.authHeader
		client.mu.RUnlock()

		outgoingPacket.ch <- client.Transport.Send(url, authHeader, outgoingPacket.packet)
	}
}

// Capture asynchronously delivers a packet to the Sentry server. It is a no-op
// when client is nil. A channel is provided if it is important to check for a
// send's success.
func (client *Client) Capture(packet *Packet, captureTags map[string]string) (eventID string, ch chan error) {
	if client == nil {
		return
	}

	ch = make(chan error, 1)

	// Merge capture tags and client tags
	packet.AddTags(captureTags)
	packet.AddTags(client.Tags)

	// Initialize any required packet fields
	client.mu.RLock()
	projectID := client.projectID
	release := client.release
	client.mu.RUnlock()

	err := packet.Init(projectID)
	if err != nil {
		ch <- err
		return
	}
	packet.Release = release

	outgoingPacket := &outgoingPacket{packet, ch}

	select {
	case client.queue <- outgoingPacket:
	default:
		// Send would block, drop the packet
		if client.DropHandler != nil {
			client.DropHandler(packet)
		}
		ch <- ErrPacketDropped
	}

	return packet.EventID, ch
}

// Capture asynchronously delivers a packet to the Sentry server with the default *Client.
// It is a no-op when client is nil. A channel is provided if it is important to check for a
// send's success.
func Capture(packet *Packet, captureTags map[string]string) (eventID string, ch chan error) {
	return DefaultClient.Capture(packet, captureTags)
}

// CaptureMessage formats and delivers a string message to the Sentry server.
func (client *Client) CaptureMessage(message string, tags map[string]string, interfaces ...Interface) string {
	if client == nil {
		return ""
	}

	packet := NewPacket(message, append(interfaces, &Message{message, nil})...)
	eventID, _ := client.Capture(packet, tags)

	return eventID
}

// CaptureMessage formats and delivers a string message to the Sentry server with the default *Client
func CaptureMessage(message string, tags map[string]string, interfaces ...Interface) string {
	return DefaultClient.CaptureMessage(message, tags, interfaces...)
}

// CaptureErrors formats and delivers an error to the Sentry server.
// Adds a stacktrace to the packet, excluding the call to this method.
func (client *Client) CaptureError(err error, tags map[string]string, interfaces ...Interface) string {
	if client == nil {
		return ""
	}

	packet := NewPacket(err.Error(), append(interfaces, NewException(err, NewStacktrace(1, 3, nil)))...)
	eventID, _ := client.Capture(packet, tags)

	return eventID
}

// CaptureErrors formats and delivers an error to the Sentry server using the default *Client.
// Adds a stacktrace to the packet, excluding the call to this method.
func CaptureError(err error, tags map[string]string, interfaces ...Interface) string {
	return DefaultClient.CaptureError(err, tags, interfaces...)
}

// CapturePanic calls f and then recovers and reports a panic to the Sentry server if it occurs.
func (client *Client) CapturePanic(f func(), tags map[string]string, interfaces ...Interface) {
	if client == nil {
		return
	}

	defer func() {
		var packet *Packet
		switch rval := recover().(type) {
		case nil:
			return
		case error:
			packet = NewPacket(rval.Error(), append(interfaces, NewException(rval, NewStacktrace(2, 3, nil)))...)
		default:
			rvalStr := fmt.Sprint(rval)
			packet = NewPacket(rvalStr, append(interfaces, NewException(errors.New(rvalStr), NewStacktrace(2, 3, nil)))...)
		}

		client.Capture(packet, tags)
	}()

	f()
}

// CapturePanic calls f and then recovers and reports a panic to the Sentry server if it occurs.
func CapturePanic(f func(), tags map[string]string, interfaces ...Interface) {
	DefaultClient.CapturePanic(f, tags, interfaces...)
}

func (client *Client) Close() {
	close(client.queue)
}

func Close() { DefaultClient.Close() }

func (client *Client) URL() string {
	client.mu.RLock()
	defer client.mu.RUnlock()

	return client.url
}

func URL() string { return DefaultClient.URL() }

func (client *Client) ProjectID() string {
	client.mu.RLock()
	defer client.mu.RUnlock()

	return client.projectID
}

func ProjectID() string { return DefaultClient.ProjectID() }

func (client *Client) Release() string {
	client.mu.RLock()
	defer client.mu.RUnlock()

	return client.release
}

func Release() string { return DefaultClient.Release() }

// HTTPTransport is the default transport, delivering packets to Sentry via the
// HTTP API.
type HTTPTransport struct {
	Http http.Client
}

func (t *HTTPTransport) Send(url, authHeader string, packet *Packet) error {
	if url == "" {
		return nil
	}

	body, contentType := serializedPacket(packet)
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("X-Sentry-Auth", authHeader)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", contentType)
	res, err := t.Http.Do(req)
	if err != nil {
		return err
	}
	io.Copy(ioutil.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("raven: got http status %d", res.StatusCode)
	}
	return nil
}

func serializedPacket(packet *Packet) (r io.Reader, contentType string) {
	packetJSON := packet.JSON()

	// Only deflate/base64 the packet if it is bigger than 1KB, as there is
	// overhead.
	if len(packetJSON) > 1000 {
		buf := &bytes.Buffer{}
		b64 := base64.NewEncoder(base64.StdEncoding, buf)
		deflate, _ := zlib.NewWriterLevel(b64, zlib.BestCompression)
		deflate.Write(packetJSON)
		deflate.Close()
		b64.Close()
		return buf, "application/octet-stream"
	}
	return bytes.NewReader(packetJSON), "application/json"
}

var hostname string

func init() {
	hostname, _ = os.Hostname()
}
