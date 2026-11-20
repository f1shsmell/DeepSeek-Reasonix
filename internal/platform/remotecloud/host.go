// Package remotecloud keeps a signed-in Studio reachable through the Reasonix
// message relay. Payloads are encrypted between the controller and Studio; the
// relay sees only routing identities, capability scopes and ciphertext.
package remotecloud

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"reasonix/internal/contract/provider"
	"reasonix/internal/platform/account"
)

// DefaultRelayURL is deliberately empty. The hosted relay this constant used to
// name is gone, and a build that falls back to it dials a host that no longer
// resolves: the user sees a thirty-second timeout instead of the one sentence
// that says what is actually missing. A deployment that wants a relay names it
// in REASONIX_REMOTE_URL; without one, the host stays offline and says so.
const DefaultRelayURL = ""

const desktopResponseChunk = 24 << 10

var (
	errAccountChanged = errors.New("remote cloud: account changed")
	errRelayRefused   = errors.New("remote cloud: relay refused the connection")
	errRelayInsecure  = errors.New("remote cloud: the relay address must start with wss://")
	errRelayUnset     = errors.New("remote cloud: REASONIX_REMOTE_URL is not set")
)

type unavailableReason string

const (
	unavailableSignedOut        unavailableReason = "signed_out"
	unavailableRelayUnset       unavailableReason = "relay_unset"
	unavailableRelayUnreachable unavailableReason = "relay_unreachable"
	unavailableRelayRefused     unavailableReason = "relay_refused"
)

type Status struct {
	DeviceID string            `json:"deviceId,omitempty"`
	Name     string            `json:"name,omitempty"`
	Online   bool              `json:"online"`
	Error    string            `json:"error,omitempty"`
	Reason   unavailableReason `json:"reason,omitempty"`
}

type hostState struct {
	status Status
}

type Host struct {
	client     *account.Client
	dialer     *websocket.Dialer
	relayURL   string
	gatewayID  string
	version    string
	name       string
	token      func() string
	tasks      TaskService
	desktop    DesktopService
	events     EventService
	presence   ControllerPresence
	muxTrace   bool
	disconnect chan controllerDisconnect

	link relayLink

	// saved reads the device identity this Studio registered with. It is a field
	// so a test can supply one; New sets it to the credential store.
	saved func() (*identity, error)

	mu    sync.RWMutex
	state hostState
}

type controllerDisconnect struct {
	id   string
	done chan error
}

type TaskService interface {
	CloudTasks(context.Context) (any, error)
	CloudTask(context.Context, string) (any, error)
	CloudSubmit(context.Context, string, string, string, int) error
}

type DesktopRequest struct {
	Method  string
	Path    string
	Body    []byte
	Ordinal int
}

type DesktopResponse struct {
	Status      int
	ContentType string
	ETag        string
	Body        []byte
}

type DesktopService interface {
	CloudDesktop(context.Context, DesktopRequest, string) (DesktopResponse, error)
}

type ControllerPresence interface {
	CloudControllerConnected(string, string) int
	CloudControllerSeen(string)
	CloudControllerDisconnected(string)
}

func New(client *account.Client, dialer *websocket.Dialer, relayURL, version string, tasks ...TaskService) *Host {
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	// No fallback host. The empty case is a deployment nobody named, and it is
	// answered by the connection attempt rather than papered over here, so the
	// status carries `relay_unset` instead of a timeout against a dead domain.
	relayURL = normalizeRelayURL(strings.TrimSpace(relayURL))
	if relayURL == "" {
		relayURL = DefaultRelayURL
	}
	name, _ := os.Hostname()
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Reasonix Studio"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	host := &Host{
		client: client, dialer: dialer, relayURL: strings.TrimRight(relayURL, "/"),
		version: version, name: name, token: account.Token, disconnect: make(chan controllerDisconnect), saved: loadIdentity,
	}
	if len(tasks) > 0 {
		host.tasks = tasks[0]
		host.desktop, _ = tasks[0].(DesktopService)
		host.events, _ = tasks[0].(EventService)
		host.presence, _ = tasks[0].(ControllerPresence)
	}
	return host
}

// DisconnectController asks the relay to close one authenticated controller.
// The live connection owns WebSocket writes, so callers hand the request to it
// instead of writing concurrently from the local HTTP handler.
// relaySecure admits wss://, and ws:// only to this machine, where nothing
// crosses a network. A device credential must never be sent in the clear.
func relaySecure(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "wss":
		return u.Host != ""
	case "ws":
		ip := net.ParseIP(u.Hostname())
		return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return false
}

// normalizeRelayURL rewrites an https:// relay origin to the wss:// a dial
// needs, the same rewrite the phone client does (src/net/relay.ts, relayUrl),
// so one grant works spelled one way. Anything else is returned untouched:
// relaySecure still decides, keeping local ws:// fixtures working.
func normalizeRelayURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	// The scheme is compared case-insensitively: an env var is typed by a
	// person, not generated as a token, and `HTTPS://` names the same service.
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return raw
	}
	if u.Path != "" && u.Path != "/" {
		return raw
	}
	// Credentials, a query and a fragment are not part of an origin. Rewriting
	// them away would dial a host the operator never named, so the string goes
	// back untouched and relaySecure refuses it with its own message.
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return raw
	}
	return "wss://" + u.Host
}

// SetGateway binds this machine to a gateway the person registered with the
// account service. The relay address is the gateway's own; the id is what the
// account service uses to scope the gateway's token to this device. Call it
// before Run.
func (h *Host) SetGateway(id string) { h.gatewayID = strings.TrimSpace(id) }

// SetMuxTrace logs each pushed frame; the host reads the env once at startup.
func (h *Host) SetMuxTrace(on bool) { h.muxTrace = on }

func (h *Host) DisconnectController(id string) error {
	id = strings.TrimSpace(id)
	if len(id) != 32 {
		return errors.New("remote cloud: invalid controller identity")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return errors.New("remote cloud: invalid controller identity")
	}
	request := controllerDisconnect{id: id, done: make(chan error, 1)}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case h.disconnect <- request:
	case <-timer.C:
		return errors.New("remote cloud: device is offline")
	}
	select {
	case err := <-request.done:
		return err
	case <-timer.C:
		return errors.New("remote cloud: disconnect timed out")
	}
}

func (h *Host) Status() Status {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.state.status
}

func (h *Host) publish(status Status) {
	h.mu.Lock()
	h.state.status = status
	h.mu.Unlock()
}

func (h *Host) Run(ctx context.Context) {
	defer h.dropControllers()
	backoff := time.Second
	for ctx.Err() == nil {
		h.expireSuspended(time.Now())
		token := strings.TrimSpace(h.token())
		if token == "" {
			h.dropControllers()
			h.publish(Status{Reason: unavailableSignedOut})
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		saved, private, err := h.ensureIdentity(ctx, token)
		if err == nil {
			err = h.connect(ctx, token, saved, private)
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errAccountChanged) {
			h.dropControllers()
			h.link.replay.reset()
		}
		h.suspend(time.Now())
		status := h.Status()
		wasOnline := status.Online
		status.Online = false
		status.Error = err.Error()
		status.Reason = unavailableReasonFor(err)
		h.publish(status)
		if wasOnline {
			backoff = time.Second
		}
		if !wait(ctx, backoff) {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func unavailableReasonFor(err error) unavailableReason {
	if errors.Is(err, account.ErrUnauthorized) {
		return unavailableSignedOut
	}
	if errors.Is(err, errRelayUnset) {
		return unavailableRelayUnset
	}
	if errors.Is(err, errRelayRefused) || errors.Is(err, errRelayInsecure) {
		return unavailableRelayRefused
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.ClosePolicyViolation {
		return unavailableRelayRefused
	}
	return unavailableRelayUnreachable
}

func (h *Host) ensureIdentity(ctx context.Context, token string) (*identity, *ecdh.PrivateKey, error) {
	user, err := h.client.Me(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	saved, err := loadIdentity()
	if err == nil && saved != nil && saved.OwnerID == user.ID {
		private, keyErr := privateKey(saved)
		if keyErr == nil && h.identityCurrent(ctx, token, saved) {
			return saved, private, nil
		}
		if keyErr == nil {
			return h.registerIdentity(ctx, token, user.ID, private)
		}
	}
	private, public, err := generatePrivateKey()
	if err != nil {
		return nil, nil, err
	}
	return h.registerIdentityWithPublic(ctx, token, user.ID, private, public)
}

func (h *Host) identityCurrent(ctx context.Context, token string, saved *identity) bool {
	devices, err := h.client.RemoteDevices(ctx, token)
	if err != nil {
		return true
	}
	for _, device := range devices {
		if device.ID == saved.DeviceID {
			return device.GatewayID == h.gatewayID && slices.Contains(device.Capabilities, account.RemoteDesktop)
		}
	}
	return false
}

func (h *Host) registerIdentity(ctx context.Context, token string, ownerID int64, private *ecdh.PrivateKey) (*identity, *ecdh.PrivateKey, error) {
	public := base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes())
	return h.registerIdentityWithPublic(ctx, token, ownerID, private, public)
}

func (h *Host) registerIdentityWithPublic(ctx context.Context, token string, ownerID int64, private *ecdh.PrivateKey, public string) (*identity, *ecdh.PrivateKey, error) {
	registered, err := h.client.RegisterRemoteDevice(ctx, token, account.RemoteDeviceRegistration{
		Name: h.name, Platform: platformName(runtime.GOOS), PublicKey: public,
		Capabilities: []account.RemoteCapability{
			account.RemoteTasks, account.RemoteLogs, account.RemoteFiles, account.RemoteDesktop,
		},
		GatewayID: h.gatewayID,
	})
	if err != nil {
		return nil, nil, err
	}
	saved := &identity{
		OwnerID: ownerID, DeviceID: registered.Device.ID,
		DeviceCredential: registered.DeviceCredential,
		PrivateKey:       base64.RawURLEncoding.EncodeToString(private.Bytes()),
	}
	if err := saveIdentity(saved); err != nil {
		return nil, nil, err
	}
	return saved, private, nil
}

func platformName(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

type gatewayMessage struct {
	Type         string                     `json:"type"`
	ConnectionID string                     `json:"connectionId"`
	Scopes       []account.RemoteCapability `json:"scopes"`
	Payload      string                     `json:"payload"`
}

type controllerCommand struct {
	Version int    `json:"v"`
	Type    string `json:"type"`
	ID      string `json:"id"`
	TaskID  string `json:"taskId,omitempty"`
	Text    string `json:"text,omitempty"`
	Method  string `json:"method,omitempty"`
	Path    string `json:"path,omitempty"`
	Body    string `json:"body,omitempty"`
	// Index and Total number the parts of an upload; see upload.go.
	Index   int    `json:"index,omitempty"`
	Total   int    `json:"total,omitempty"`
	Session string `json:"session,omitempty"`
	Seq     int64  `json:"seq,omitempty"`
	// Runtime, After, Attach and Credit drive the mux push lane; see mux.go.
	Runtime string `json:"runtime,omitempty"`
	After   int64  `json:"after,omitempty"`
	Attach  bool   `json:"attach,omitempty"`
	Credit  int64  `json:"credit,omitempty"`
}

func (h *Host) connect(ctx context.Context, token string, saved *identity, private *ecdh.PrivateKey) error {
	if h.relayURL == "" {
		return errRelayUnset
	}
	if !relaySecure(h.relayURL) {
		return errRelayInsecure
	}
	url := h.relayURL + "/v1/devices/" + saved.DeviceID + "/connect"
	headers := http.Header{
		"Authorization": []string{"Bearer " + saved.DeviceCredential},
		"User-Agent":    []string{h.userAgent()},
	}
	conn, response, err := h.dialer.DialContext(ctx, url, headers)
	if err != nil {
		return classifyRelayDialError(response, err)
	}
	defer conn.Close()
	conn.SetReadLimit(64 << 10)
	h.publish(Status{DeviceID: saved.DeviceID, Name: h.name, Online: true})

	done := make(chan struct{})
	defer close(done)
	messages := make(chan []byte, 1)
	readErr := make(chan error, 1)
	go readRelay(conn, messages, readErr, done)

	sessions := h.adopt(saved.DeviceID)
	writer := &relayWriter{conn: conn}
	// A lost connection ends every push subscription; the sessions themselves
	// stay resumable for the grace period, and the controller reopens streams.
	defer func() {
		for _, session := range sessions {
			session.cancelPush()
		}
	}()
	pingTicker := time.NewTicker(20 * time.Second)
	tokenTicker := time.NewTicker(time.Second)
	heartbeat := ""
	defer pingTicker.Stop()
	defer tokenTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case <-tokenTicker.C:
			if strings.TrimSpace(h.token()) != token {
				return errAccountChanged
			}
		case <-pingTicker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return err
			}
			if heartbeat != "" {
				if err := writer.write([]byte(heartbeat)); err != nil {
					return err
				}
			}
		case request := <-h.disconnect:
			if _, ok := sessions[request.id]; !ok {
				request.done <- errors.New("remote cloud: controller is no longer connected")
				continue
			}
			wire, err := json.Marshal(map[string]string{
				"type": "disconnect_controller", "connectionId": request.id,
			})
			if err == nil {
				err = writer.write(wire)
			}
			if err == nil {
				h.forgetController(request.id)
			}
			request.done <- err
		case payload := <-messages:
			if hello, ok := parseRelayHello(payload); ok {
				h.keepControllers(hello.Controllers)
				if hello.Heartbeat != "" {
					heartbeat = hello.Heartbeat
					pingTicker.Reset(hello.interval())
				}
				continue
			}
			if err := h.handle(ctx, writer, saved, private, sessions, payload); err != nil {
				continue
			}
		}
	}
}

// readRelay pumps the socket into messages; a read error or a binary frame
// ends the connection through readErr.
func readRelay(conn *websocket.Conn, messages chan<- []byte, readErr chan<- error, done <-chan struct{}) {
	for {
		kind, payload, err := conn.ReadMessage()
		if err != nil {
			select {
			case readErr <- err:
			case <-done:
			}
			return
		}
		if kind != websocket.TextMessage {
			select {
			case readErr <- errors.New("remote cloud: relay sent a binary message"):
			case <-done:
			}
			return
		}
		select {
		case messages <- payload:
		case <-done:
			return
		}
	}
}

func classifyRelayDialError(response *http.Response, err error) error {
	if response == nil {
		return err
	}
	if response.StatusCode == http.StatusUnauthorized {
		_ = clearIdentity()
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 {
		return fmt.Errorf("%w: %w", errRelayRefused, err)
	}
	return err
}

func (h *Host) handle(
	ctx context.Context,
	writer *relayWriter,
	saved *identity,
	private *ecdh.PrivateKey,
	sessions map[string]*controllerSession,
	payload []byte,
) error {
	var message gatewayMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return err
	}
	switch message.Type {
	case "controller_disconnected":
		if h.presence != nil {
			h.presence.CloudControllerDisconnected(message.ConnectionID)
		}
		if session := sessions[message.ConnectionID]; session != nil {
			session.stop()
		}
		delete(sessions, message.ConnectionID)
		return nil
	case "controller_connected", "heartbeat_ack":
		return nil
	case "controller_message":
	default:
		return errors.New("remote cloud: unknown relay message")
	}
	session := sessions[message.ConnectionID]
	if session == nil {
		created, err := newSessionCipher(private, saved.DeviceID, message.Payload)
		if err != nil {
			return err
		}
		ordinal := 0
		if h.presence != nil {
			ordinal = h.presence.CloudControllerConnected(message.ConnectionID, "Web Studio")
		}
		nonce, err := randomToken()
		if err != nil {
			return err
		}
		push := helloMux(message.Payload)
		session = &controllerSession{
			cipher: created, ordinal: ordinal, nonce: nonce,
			connectionID: message.ConnectionID, deviceID: "cloud:" + message.ConnectionID,
			commands: make(chan sessionCommand, 256), done: make(chan struct{}),
			push: &muxPush{enabled: push, subs: make(map[string]*muxSub)},
		}
		sessions[message.ConnectionID] = session
		go h.runSession(ctx, session)
		ready := map[string]any{
			"v": protocolVersion, "type": "ready", "deviceId": saved.DeviceID,
			"name": h.name, "platform": platformName(runtime.GOOS), "version": h.version,
			"features": []string{"replay", "upload"}, "session": nonce, "instance": h.instance(),
		}
		if h.events != nil && push {
			ready["capabilities"] = map[string]int{"mux": 1}
		}
		reply, err := created.seal(ready)
		if err != nil {
			return err
		}
		return writeDirected(writer, message.ConnectionID, reply)
	}
	var command controllerCommand
	if h.presence != nil {
		h.presence.CloudControllerSeen(message.ConnectionID)
	}
	if err := session.cipher.open(message.Payload, &command); err != nil {
		return err
	}
	if command.Version != protocolVersion || command.ID == "" {
		return errors.New("remote cloud: unsupported controller command")
	}
	if err := session.admit(command); err != nil {
		return err
	}
	if command.Type == "mux.credit" {
		// Grant before the queue: a credit that waits behind a slow desktop
		// request would let the credit timeout starve this session's streams.
		h.muxCredit(session, writer, message.Scopes, command)
		return nil
	}
	// A full queue means commands arrive faster than they run; dropping is
	// recoverable through the controller's request timeout, blocking the read
	// loop would stall every other controller on this device.
	select {
	case session.commands <- sessionCommand{writer: writer, scopes: message.Scopes, command: command}:
	case <-session.done:
	default:
	}
	return nil
}

func (h *Host) desktopCommand(
	ctx context.Context,
	writer *relayWriter,
	session *sessionCipher,
	deviceID string,
	ordinal int,
	connectionID string,
	scopes []account.RemoteCapability,
	command controllerCommand,
) error {
	if h.desktop == nil || !hasScope(scopes, account.RemoteDesktop) {
		return h.writeDesktopError(writer, session, connectionID, command.ID, "desktop access is unavailable")
	}
	body, err := base64.RawURLEncoding.DecodeString(command.Body)
	if err != nil {
		return h.writeDesktopError(writer, session, connectionID, command.ID, "desktop request body is invalid")
	}
	return h.runDesktop(ctx, writer, session, deviceID, ordinal, connectionID, command.ID, DesktopRequest{Method: command.Method, Path: command.Path, Body: body})
}

// uploadPart takes one part of a request too large for one message and runs the
// whole once the last arrives. The scope is checked on every part, so a link
// that lost it mid-upload stops there.
func (h *Host) uploadPart(
	ctx context.Context,
	writer *relayWriter,
	session *controllerSession,
	deviceID string,
	connectionID string,
	scopes []account.RemoteCapability,
	command controllerCommand,
) error {
	if h.desktop == nil || !hasScope(scopes, account.RemoteDesktop) {
		session.uploads.drop(command.ID)
		return h.writeDesktopError(writer, session.cipher, connectionID, command.ID, "desktop access is unavailable")
	}
	request, done, err := session.uploads.add(command, time.Now())
	if err != nil {
		return h.writeDesktopError(writer, session.cipher, connectionID, command.ID, err.Error())
	}
	if !done {
		return nil
	}
	return h.runDesktop(ctx, writer, session.cipher, deviceID, session.ordinal, connectionID, command.ID, request)
}

// runDesktop is the one door a desktop request, whole or reassembled, goes
// through: the answer to a state-changing request is kept, so a resend of the
// same request returns it instead of running the request again.
func (h *Host) runDesktop(
	ctx context.Context,
	writer *relayWriter,
	session *sessionCipher,
	deviceID string,
	ordinal int,
	connectionID string,
	id string,
	request DesktopRequest,
) error {
	replay := replayable(request.Method)
	key := replayKey(id, request.Method, request.Path, request.Body)
	if replay {
		if payloads, ok := h.link.replay.get(key, time.Now()); ok {
			return writeSealed(writer, session, connectionID, payloads)
		}
	}
	request.Ordinal = ordinal
	response, err := h.desktop.CloudDesktop(ctx, request, deviceID)
	if err != nil {
		payloads := []map[string]any{{"v": protocolVersion, "type": "error", "id": id, "error": err.Error()}}
		if replay {
			h.link.replay.put(key, payloads, len(err.Error()), time.Now())
		}
		return writeSealed(writer, session, connectionID, payloads)
	}
	payloads := desktopPayloads(id, response)
	if replay {
		h.link.replay.put(key, payloads, len(response.Body), time.Now())
	}
	return writeSealed(writer, session, connectionID, payloads)
}

func desktopPayloads(id string, response DesktopResponse) []map[string]any {
	chunks := max((len(response.Body)+desktopResponseChunk-1)/desktopResponseChunk, 1)
	payloads := make([]map[string]any, 0, chunks)
	for index := range chunks {
		start := index * desktopResponseChunk
		end := min(start+desktopResponseChunk, len(response.Body))
		chunk := ""
		if start < len(response.Body) {
			chunk = base64.RawURLEncoding.EncodeToString(response.Body[start:end])
		}
		payloads = append(payloads, map[string]any{
			"v": protocolVersion, "type": "desktop.response", "id": id,
			"status": response.Status, "contentType": response.ContentType, "etag": response.ETag,
			"index": index, "done": index == chunks-1, "body": chunk,
		})
	}
	return payloads
}

func writeSealed(writer *relayWriter, session *sessionCipher, connectionID string, payloads []map[string]any) error {
	for _, payload := range payloads {
		reply, err := session.seal(payload)
		if err != nil {
			return err
		}
		if err := writeDirected(writer, connectionID, reply); err != nil {
			return err
		}
	}
	return nil
}

func (h *Host) writeDesktopError(writer *relayWriter, session *sessionCipher, connectionID, id, message string) error {
	reply, err := session.seal(map[string]any{
		"v": protocolVersion, "type": "error", "id": id, "error": message,
	})
	if err != nil {
		return err
	}
	return writeDirected(writer, connectionID, reply)
}

func (h *Host) taskCommand(ctx context.Context, deviceID string, ordinal int, scopes []account.RemoteCapability, command controllerCommand) map[string]any {
	response := map[string]any{"v": protocolVersion, "id": command.ID}
	if h.tasks == nil || !hasScope(scopes, account.RemoteTasks) {
		response["type"] = "error"
		response["error"] = "tasks access is unavailable"
		return response
	}
	var value any
	var err error
	switch command.Type {
	case "tasks.list":
		value, err = h.tasks.CloudTasks(ctx)
		response["type"] = "tasks.list"
		response["tasks"] = value
	case "tasks.get":
		value, err = h.tasks.CloudTask(ctx, command.TaskID)
		response["type"] = "tasks.get"
		response["snapshot"] = value
	case "tasks.send":
		err = h.tasks.CloudSubmit(ctx, command.TaskID, command.Text, deviceID, ordinal)
		response["type"] = "tasks.sent"
	}
	if err != nil {
		response["type"] = "error"
		response["error"] = err.Error()
	}
	return response
}

func hasScope(scopes []account.RemoteCapability, want account.RemoteCapability) bool {
	return slices.Contains(scopes, want)
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// userAgent is the identity the account client already signs in with, so the
// relay handshake and the sign-in reach the same edge as the same caller.
func (h *Host) userAgent() string {
	if h.client != nil && h.client.UserAgent != "" {
		return h.client.UserAgent
	}
	return provider.ClientUserAgent()
}
