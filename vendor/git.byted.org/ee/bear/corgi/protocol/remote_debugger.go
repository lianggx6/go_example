package protocol

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gobs/httpclient"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	// ErrorNoActiveTab is returned if there are no active tabs (of type "page")
	ErrorNoActiveTab = errors.New("no active tab")
	// ErrorNoWsURL is returned if the active tab has no websocket URL
	ErrorNoWsURL = errors.New("no websocket URL")
	// ErrorNoResponse is returned if a method was expecting a response but got nil instead
	ErrorNoResponse = errors.New("no response")

	MaxReadBufferSize  = 0                // default gorilla/websocket buffer size
	MaxWriteBufferSize = 30 * 1024 * 1024 // this should be large enough to send large scripts

	ErrCodeGetTabNumFaied = -10
)

// ErrorReason defines what error should be generated to abort a request in ContinueInterceptedRequest
type ErrorReason string

// Tab represents an opened tab/page.
type Tab struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	WsURL       string `json:"webSocketDebuggerUrl"`
	DevURL      string `json:"devtoolsFrontendUrl"`
}

// EvaluateError is returned by Evaluate in case of expression errors.
type EvaluateError struct {
	ErrorDetails     map[string]interface{}
	ExceptionDetails map[string]interface{}
}

func (err EvaluateError) Error() string {
	desc := err.ErrorDetails["description"].(string)
	if excp := err.ExceptionDetails; excp != nil {
		if excp["exception"] != nil {
			desc += fmt.Sprintf(" at line %v col %v",
				excp["lineNumber"].(float64), excp["columnNumber"].(float64))
		}
	}

	return desc
}

// RemoteDebugger implements an interface for Chrome DevTools.
type RemoteDebugger struct {
	http    *httpclient.HttpClient
	ws      *websocket.Conn
	current string // tab ID
	reqID   int
	verbose bool

	sync.Mutex
	Closed      chan bool // 连接关闭的信号量
	isAvailable bool      // 连接关闭的标识位

	requests  chan Params
	responses map[int]chan json.RawMessage
	callbacks map[string]EventCallback
	events    chan wsMessage
}

type Clip struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	Scale  float32 `json:"scale"`
}

const (
	ResourceTypeDocument    = "Document"
	ResourceTypeXHR         = "XHR"
	ResourceTypeImg         = "Image"
	ResourceTypeWebSocket   = "WebSocket"
	ResourceTypeStylesheet  = "Stylesheet"
	ResourceTypeMedia       = "Media"
	ResourceTypeFont        = "Font"
	ResourceTypeScript      = "Script"
	ResourceTypeTextTrack   = "TextTrack"
	ResourceTypeFetch       = "Fetch"
	ResourceTypeEventSource = "EventSource"
	ResourceTypeManifest    = "Manifest"
	ResourceTypeOther       = "Other"

	ErrorReasonAbort                = "Aborted"
	ErrorReasonFailed               = "Failed"
	ErrorReasonTimedOut             = "TimedOut"
	ErrorReasonInternetDisconnected = "InternetDisconnected"
)

var statusText = map[string]string{
	"100": "Continue",
	"101": "Switching Protocols",
	"102": "Processing",
	"200": "OK",
	"201": "Created",
	"202": "Accepted",
	"203": "Non-Authoritative Information",
	"204": "No Content",
	"206": "Partial Content",
	"207": "Multi-Status",
	"208": "Already Reported",
	"209": "IM Used",
	"300": "Multiple Choices",
	"301": "Moved Permanently",
	"302": "Found",
	"303": "See Other",
	"304": "Not Modified",
	"305": "Use Proxy",
	"306": "Switch Proxy",
	"307": "Temporary Redirect",
	"308": "Permanent Redirect",
	"400": "Bad Request",
	"401": "Unauthorized",
	"402": "Payment Required",
	"403": "Forbidden",
	"404": "Not Found",
	"405": "Method Not Allowed",
	"406": "Not Acceptable",
	"407": "Proxy Authentication Required",
	"408": "Request Timeout",
	"409": "Conflict",
	"410": "Gone",
	"411": "Length Required",
	"412": "Precondition Failed",
	"413": "Payload Too Large",
	"414": "URI Too Long",
	"415": "Unsupported Media Type",
	"416": "Range Not Satisfiable",
	"417": "Expectation Failed",
	"418": "I\"m a teapot",
	"421": "Misdirected Request",
	"422": "Unprocessable Entity",
	"423": "Locked",
	"424": "Failed Dependency",
	"426": "Upgrade Required",
	"428": "Precondition Required",
	"429": "Too Many Requests",
	"431": "Request Header Fields Too Large",
	"451": "Unavailable For Legal Reasons",
	"500": "Internal Server Error",
	"501": "Not Implemented",
	"502": "Bad Gateway",
	"503": "Service Unavailable",
	"504": "Gateway Timeout",
	"505": "HTTP Version Not Supported",
	"506": "Variant Also Negotiates",
	"507": "Insufficient Storage",
	"508": "Loop Detected",
	"510": "Not Extended",
	"511": "Network Authentication Required",
}

// Params is a type alias for the event params structure.
type Params map[string]interface{}

func (p Params) String(k string) string {
	return p[k].(string)
}

func (p Params) Int(k string) int {
	return int(p[k].(float64))
}

func (p Params) Map(k string) map[string]interface{} {
	return p[k].(map[string]interface{})
}

// EventCallback represents a callback event, associated with a method.
type EventCallback func(params Params)

func decode(resp *httpclient.HttpResponse, v interface{}) error {
	err := json.NewDecoder(resp.Body).Decode(v)
	resp.Close()

	return err
}

func unmarshal(payload []byte) (map[string]interface{}, error) {
	var response map[string]interface{}
	err := json.Unmarshal(payload, &response)
	if err != nil {
	}
	return response, err
}

func responseError(resp *httpclient.HttpResponse, err error) (*httpclient.HttpResponse, error) {
	if err == nil {
		return resp, resp.ResponseError()
	}

	return resp, err
}

// Connect to the remote debugger and return `RemoteDebugger` object.
func Connect(host string) (*RemoteDebugger, error) {
	remote := &RemoteDebugger{
		http:        httpclient.NewHttpClient("http://" + host),
		requests:    make(chan Params),
		responses:   map[int]chan json.RawMessage{},
		callbacks:   map[string]EventCallback{},
		events:      make(chan wsMessage, 256),
		Closed:      make(chan bool),
		isAvailable: false,
	}

	remote.http.SetTimeout(8 * time.Second)
	if _, err := remote.NewTab(""); err != nil {
		return nil, err
	}

	remote.isAvailable = true
	go remote.sendMessages()
	go remote.processEvents()
	go remote.waitForClose()

	return remote, nil
}

func (remote *RemoteDebugger) waitForClose() {

	select {
	case <-remote.Closed:
		remote.destructor()
	}
}

// Close the RemoteDebugger connection.
func (remote *RemoteDebugger) Close() {
	if remote.IsAvailable() {
		close(remote.Closed)
	}
}

func (remote *RemoteDebugger) destructor() {

	remote.Lock()
	ws := remote.ws
	remote.ws = nil
	remote.isAvailable = false
	remote.Unlock()

	if ws != nil { // already closed
		ws.Close()
	}

	remote.CloseTab(remote.current)
}

func (remote *RemoteDebugger) enqueueEmptyEvent() {
	ev := wsMessage{}
	remote.events <- ev
}

func (remote *RemoteDebugger) connectWs(tab *Tab) error {
	if tab == nil {
		return ErrorNoActiveTab
	}

	if remote.ws != nil {
		if tab.ID == remote.current {
			// nothing to do
			return nil
		}

		remote.Lock()
		ws := remote.ws
		remote.ws, remote.current = nil, ""
		remote.Unlock()

		_ = ws.Close()
	}

	if len(tab.WsURL) == 0 {
		return ErrorNoWsURL
	}

	log.Debug("connecting to tab", tab.WsURL)

	d := &websocket.Dialer{
		ReadBufferSize:  MaxReadBufferSize,
		WriteBufferSize: MaxWriteBufferSize,
	}

	ws, _, err := d.Dial(tab.WsURL, nil)
	if err != nil {
		log.Error("dial error:", err)
		return err
	}

	remote.Lock()
	remote.ws = ws
	remote.current = tab.ID
	remote.Unlock()

	go remote.readMessages()
	return nil
}

func (remote *RemoteDebugger) socket() (ws *websocket.Conn) {
	remote.Lock()
	ws = remote.ws
	remote.Unlock()
	return
}

type wsMessage struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`

	Method string          `json:"Method"`
	Params json.RawMessage `json:"Params"`
}

// SendRequest sends a request and returns the reply as a a map.
func (remote *RemoteDebugger) SendRequest(method string, params Params) (map[string]interface{}, error) {
	rawReply, err := remote.sendRawReplyRequest(method, params)
	if err != nil || rawReply == nil {
		return nil, err
	}
	return unmarshal(rawReply)
}

// sendRawReplyRequest sends a request and returns the reply bytes.
func (remote *RemoteDebugger) sendRawReplyRequest(method string, params Params) ([]byte, error) {
	responseChann := make(chan json.RawMessage, 1)
	remote.Lock()
	reqID := remote.reqID
	remote.responses[reqID] = responseChann
	remote.reqID++
	remote.Unlock()

	command := Params{
		"id":     reqID,
		"method": method,
		"params": params,
	}

	select {
	case remote.requests <- command:
		break
	case <-time.After(time.Second * 120):
		return nil, fmt.Errorf("request timeout, tabID: %s, request:%+v", remote.current, command)
	}

	var reply json.RawMessage
	select {
	case reply = <-responseChann:
		break
	case <-time.After(time.Second * 120):
		return nil, fmt.Errorf("response timeout, tabID: %s, request:%+v", remote.current, command)
	}

	remote.Lock()
	delete(remote.responses, reqID)
	remote.Unlock()

	return reply, nil
}

func (remote *RemoteDebugger) sendMessages() {

	for {
		select {
		case <-remote.Closed:
			return
		case message := <-remote.requests:
			bytes, err := json.Marshal(message)
			if err != nil {
				log.Errorf("marshal message fail, message: %+v, error: %s, tabID: %s", message, err, remote.current)
				continue
			}

			ws := remote.socket()
			if ws != nil {
				err = ws.WriteMessage(websocket.TextMessage, bytes)
				if err != nil {
					log.Errorf("write message fail, message: %+v, error: %s, tabID: %s", message, err, remote.current)
				}
				log.Infof("write message success, message: %+v, tabID: %s", message, remote.current) //temp
			}
		}
	}
}

func permanentError(err error) bool {
	if websocket.IsUnexpectedCloseError(err) {
		return true
	}

	if neterr, ok := err.(net.Error); ok && !neterr.Temporary() {
		return true
	}

	return false
}

func (remote *RemoteDebugger) readMessages() {

	defer close(remote.events)

loop:
	for {
		select {
		case <-remote.Closed:
			break loop
		default:
			ws := remote.socket()
			if ws == nil {
				break loop
			}
			_, bytes, err := ws.ReadMessage()
			if err != nil {
				log.Infof("ws ReadMessage error: %s, tabID: %s", err, remote.current)
				if permanentError(err) {
					break loop
				}
			} else {
				var message wsMessage
				if err := json.Unmarshal(bytes, &message); err != nil {
					log.Errorf("unmarshal fail, data: %s, length: %d, error: %s, tabID: %s", string(bytes), len(bytes), err, remote.current)
				} else if message.Method != "" {
					remote.logMessage("receive request message,", message) //temp
					remote.Lock()
					_, ok := remote.callbacks[message.Method]
					remote.Unlock()

					if !ok {
						continue // don't queue unrequested events
					}
					select {
					case <-remote.Closed:
						// "channel closing principle": close channel in sender. or it would panic with "send on closed channel"
						//close(remote.events)
						break loop
					default:
						remote.events <- message
					}

				} else {
					remote.logMessage("receive response message,", message) //temp
					remote.Lock()
					ch := remote.responses[message.ID]
					remote.Unlock()

					if ch != nil {
						ch <- message.Result
					}
				}
			}
		}
	}
	log.Infof("goroutine of readMessages quit, tabID: %s", remote.current)
}

func (remote *RemoteDebugger) logMessage(pre string, message wsMessage) {
	result, _ := unmarshal(message.Result)
	param, _ := unmarshal(message.Params)
	log.Infof(pre+" tabID: %s, reqID: %d, method: %s, result: %+v, param: %+v", remote.current, message.ID,
		message.Method, result, param)
}

func (remote *RemoteDebugger) processEvents() {
	for ev := range remote.events {
		select {
		case <-remote.Closed:
			return
		default:
			remote.Lock()
			cb := remote.callbacks[ev.Method]
			remote.Unlock()

			if cb != nil {
				var params Params
				if err := json.Unmarshal(ev.Params, &params); err != nil {
					log.Errorf("unmarshal fail, data: %s, length: %d, error: %s, tabID: %s",
						string(ev.Params), len(ev.Params), err, remote.current)
				} else {
					remote.logMessage("receive event,", ev)
					cb(params)
				}
			}
		}
	}
}

// CallbackEvent sets a callback for the specified event.
func (remote *RemoteDebugger) CallbackEvent(method string, cb EventCallback) {
	remote.Lock()
	remote.callbacks[method] = cb
	remote.Unlock()
}

func (remote *RemoteDebugger) ResetCallbackEvent(methods ...string) {
	remote.Lock()
	for _, method := range methods {
		delete(remote.callbacks, method)
	}
	remote.Unlock()

}

// DomainEvents enables event listening in the specified domain.
func (remote *RemoteDebugger) DomainEvents(domain string, enable bool) error {
	method := domain

	if enable {
		method += ".enable"
	} else {
		method += ".disable"
	}

	_, err := remote.SendRequest(method, nil)
	return err
}

// PageEvents enables Page events listening.
func (remote *RemoteDebugger) PageEvents(enable bool) error {
	return remote.DomainEvents("Page", enable)
}

// NetworkEvents enables Network events listening.
func (remote *RemoteDebugger) NetworkEvents(enable bool) error {
	return remote.DomainEvents("Network", enable)
}

// RuntimeEvents enables Runtime events listening.
func (remote *RemoteDebugger) RuntimeEvents(enable bool) error {
	return remote.DomainEvents("Runtime", enable)
}

// EmulationEvents enables Emulation events listening.
func (remote *RemoteDebugger) EmulationEvents(enable bool) error {
	return remote.DomainEvents("Emulation", enable)
}

// LogEvents enables Log events listening.
func (remote *RemoteDebugger) LogEvents(enable bool) error {
	return remote.DomainEvents("Log", enable)
}

// Console enables.
func (remote *RemoteDebugger) ConsoleEvents(enable bool) error {
	return remote.DomainEvents("Console", enable)
}

// TracingEvents enables Emulation events listening.
func (remote *RemoteDebugger) TracingEvents(enable bool) error {
	return remote.DomainEvents("Tracing", enable)
}

func (remote *RemoteDebugger) TracingStart(bufferUsageReportingInterval int64) {
	_, err := remote.SendRequest("Tracing.start", Params{
		"bufferUsageReportingInterval": bufferUsageReportingInterval,
	})
	if err != nil {
		log.Warnf("Tracing.start failed:%s", err)
	}
}

func (remote *RemoteDebugger) TracingEnd() {
	_, _ = remote.SendRequest("Tracing.end", Params{})
}

func (remote *RemoteDebugger) TracingGetCategories() ([]string, error) {
	res, err := remote.SendRequest("Tracing.getCategories", Params{})
	if err != nil {
		log.Warnf("Tracing.getCategories failed:%s", err)
		return nil, err
	}
	categories, ok := res["categories"]
	if !ok {
		return nil, errors.New("no categories")
	}
	return categories.([]string), nil
}

func (remote *RemoteDebugger) TracingRequestMemoryDump() (dumpGuid string, err error) {
	res, err := remote.SendRequest("Tracing.requestMemoryDump", Params{})
	if err != nil {
		return "", err
	}
	success, ok := res["success"]
	if !ok {
		return "", errors.New("invalid response")
	}
	succ, ok := success.(bool)
	if !ok || !succ {
		return "", errors.New("failed")
	}

	gUid, ok := res["dumpGuid"]
	dumpGuid, ok = gUid.(string)
	if !ok {
		return "", errors.New("failed")
	}
	return
}

// TabList returns a list of opened tabs/pages.
// If filter is not empty, only tabs of the specified type are returned (i.e. "page").
//
// Note that tabs are ordered by activitiy time (most recently used first) so the
// current tab is the first one of type "page".
func (remote *RemoteDebugger) TabList(filter string) ([]*Tab, error) {
	resp, err := responseError(remote.http.Get("/json/list", nil, nil))
	if err != nil {
		return nil, err
	}

	var tabs []*Tab

	if err = decode(resp, &tabs); err != nil {
		return nil, err
	}

	if filter == "" {
		return tabs, nil
	}

	var filtered []*Tab

	for _, t := range tabs {
		if t.Type == filter {
			filtered = append(filtered, t)
		}
	}

	return filtered, nil
}

func GetTabList(host string) ([]*Tab, error) {
	cl := httpclient.NewHttpClient("http://" + host)
	cl.SetTimeout(2 * time.Second)
	resp, err := responseError(cl.Get("/json/list", nil, nil))
	if err != nil {
		return nil, err
	}

	var tabs []*Tab
	if err = decode(resp, &tabs); err != nil {
		return nil, err
	}
	return tabs, nil
}

func GetTabNum(host string) (int, error) {
	tabs, err := GetTabList(host)
	if err != nil {
		log.Warnf("get tab list failed:%s", err)
		return ErrCodeGetTabNumFaied, err
	}
	return len(tabs) - 1, nil
}

func (remote *RemoteDebugger) GetTabID() string {
	return remote.current
}

// ContinueInterceptedRequest is the response to Network.requestIntercepted
// which either modifies the request to continue with any modifications, or blocks it,
// or completes it with the provided response bytes.
//
// If a network fetch occurs as a result which encounters a redirect an additional Network.requestIntercepted
// event will be sent with the same InterceptionId.
//
// Parameters:
//  errorReason ErrorReason - if set this causes the request to fail with the given reason.
//  rawResponse string - if set the requests completes using with the provided base64 encoded raw response, including HTTP status line and headers etc...
//  url string - if set the request url will be modified in a way that's not observable by page.
//  method string - if set this allows the request method to be overridden.
//  postData string - if set this allows postData to be set.
//  headers Headers - if set this allows the request headers to be changed.
func (remote *RemoteDebugger) ContinueInterceptedRequest(interceptionID string,
	errorReason ErrorReason,
	rawResponse string,
	url string,
	method string,
	postData string,
	headers map[string]string) error {
	params := Params{
		"interceptionId": interceptionID,
	}

	if errorReason != "" {
		params["errorReason"] = string(errorReason)
	}
	if rawResponse != "" {
		params["rawResponse"] = rawResponse
	}
	if url != "" {
		params["url"] = url
	}
	if method != "" {
		params["method"] = method
	}
	if postData != "" {
		params["postData"] = postData
	}
	if headers != nil {
		params["headers"] = headers
	}

	_, err := remote.SendRequest("Network.continueInterceptedRequest", params)
	return err
}

// 组装base64格式的HTTP报文
// status: int
// header: map
// contentType: string
// body: []byte
// params格式：
// {
//     status: 404,
//     headers: {"foo": "bar"}
//     contentType: 'text/plain',
//     body: 'Not Found!'
//  }
func (remote *RemoteDebugger) ConbineHttpRawResponse(params Params) string {

	responseHeader := map[string]string{}
	headers := params["headers"].(map[string]string)
	for k, v := range headers {
		responseHeader[strings.ToLower(k)] = v
	}
	contentType := params.String("contentType")
	if contentType != "" {
		responseHeader["content-type"] = contentType
	}

	responseBody := params["body"].([]byte)
	statusCode := fmt.Sprintf("%d", params["status"].(int))
	statusLine := "HTTP/1.1" + statusCode + statusText[statusCode]
	CRLF := "\r\n"

	responseHeader["content-length"] = fmt.Sprintf("%d", len(responseBody))

	text := statusLine + CRLF
	for k, v := range responseHeader {
		text += k + ": " + v + CRLF
	}
	text += CRLF
	text += string(responseBody)
	rawResponse := base64.StdEncoding.EncodeToString([]byte(text))
	return rawResponse
}

// GetResponseBody returns the response body of a given requestId (from the Network.responseReceived payload).
func (remote *RemoteDebugger) GetResponseBody(req string) ([]byte, error) {
	res, err := remote.SendRequest("Network.getResponseBody", Params{
		"requestId": req,
	})

	if err != nil {
		return nil, err
	} else if b, ok := res["base64Encoded"]; ok && b.(bool) {
		return base64.StdEncoding.DecodeString(res["body"].(string))
	} else {
		return []byte(res["body"].(string)), nil
	}
}

// EnableRequestInterception enables interception, modification or cancellation of network requests
func (remote *RemoteDebugger) EnableRequestInterception(patterns []Params) error {

	_, err := remote.SendRequest("Network.setRequestInterception", Params{
		"patterns": patterns,
	})

	if err != nil {
	}

	return err
}

// 向chrome发送截图请求。 chrome返回经过base64编码的string
func (remote *RemoteDebugger) CaptureScreenshot(format string, clip Clip, quality int, fromSurface bool) (string, error) {
	if format == "" {
		format = "png"
	}

	res, err := remote.SendRequest("Page.captureScreenshot", Params{
		"format":      format,
		"quality":     quality,
		"clip":        clip,
		"fromSurface": fromSurface,
	})

	if err != nil {
		return "", err
	}

	if res == nil {
		return "", ErrorNoResponse
	}

	return res["data"].(string), nil
}

// SetControlNavigations toggles navigation throttling which allows programatic control over navigation and redirect response.
func (remote *RemoteDebugger) SetControlNavigations(enabled bool) error {
	_, err := remote.SendRequest("Page.setControlNavigations", Params{
		"enabled": enabled,
	})

	return err
}

func (remote *RemoteDebugger) SetDeviceMetricsOverride(width float64, height float64, deviceScaleFactor int) error {
	_, err := remote.SendRequest("Emulation.setDeviceMetricsOverride", Params{
		"width":             width,
		"height":            height,
		"screenWidth":       width,
		"screenHeight":      height,
		"deviceScaleFactor": deviceScaleFactor,
		"scale":             2,
		"viewport": Params{
			"x":      0,
			"y":      0,
			"width":  width * 2,
			"height": height * 2,
			"scale":  2,
		},
	})
	return err
}

// Navigate navigates to the specified URL.
func (remote *RemoteDebugger) Navigate(url string) (string, error) {
	res, err := remote.SendRequest("Page.navigate", Params{
		"url": url,
	})
	if err != nil {
		return "", err
	}

	frameID, ok := res["frameId"]
	if !ok {
		return "", nil
	}
	return frameID.(string), nil
}

func (remote *RemoteDebugger) SetCookie(param Params) error {
	_, err := remote.SendRequest("Network.setCookie", param)
	return err
}

func (remote *RemoteDebugger) GetLayoutMetrics() (map[string]interface{}, error) {

	res, err := remote.SendRequest("Page.getLayoutMetrics", Params{})
	if err != nil {
		return nil, err
	}
	return res["contentSize"].(map[string]interface{}), err
}

// NewTab creates a new tab. A tab owes a websocket connection
func (remote *RemoteDebugger) NewTab(url string) (*Tab, error) {
	path := "/json/new"
	if url != "" {
		path += "?" + url
	}

	resp, err := responseError(remote.http.Do(remote.http.Request("GET", path, nil, nil)))
	if err != nil {
		return nil, err
	}

	var tab Tab
	if err = decode(resp, &tab); err != nil {
		return nil, err
	}

	if err = remote.connectWs(&tab); err != nil {
		return nil, err
	}

	return &tab, nil
}

// CloseTab closes the specified tab.
func (remote *RemoteDebugger) CloseTab(tabID string) error {
	resp, err := responseError(remote.http.Get("/json/close/"+tabID, nil, nil))
	resp.Close()
	return err
}

func (remote *RemoteDebugger) Activate() error {
	resp, err := responseError(remote.http.Get("/json/activate/"+remote.current, nil, nil))
	resp.Close()
	return err
}

func (remote *RemoteDebugger) IsAvailable() bool {
	remote.Lock()
	isAvailable := remote.isAvailable
	remote.Unlock()
	return isAvailable
}

// Evaluate evalutes a Javascript function in the context of the current page.
func (remote *RemoteDebugger) Evaluate(expr string) (interface{}, error) {
	res, err := remote.SendRequest("Runtime.evaluate", Params{
		"expression":    expr,
		"returnByValue": true,
	})

	if err != nil {
		return nil, err
	}

	if res == nil {
		return nil, nil
	}

	result := res["result"].(map[string]interface{})
	if subtype, ok := result["subtype"]; ok && subtype.(string) == "error" {
		// this is actually an error
		exception := res["exceptionDetails"].(map[string]interface{})
		return nil, EvaluateError{ErrorDetails: result, ExceptionDetails: exception}
	}

	return result["value"], nil
}

// PrintToPDFOption defines the functional option for PrintToPDF
type PrintToPDFOption func(map[string]interface{})

// LandscapeMode instructs PrintToPDF to print pages in landscape mode
func LandscapeMode() PrintToPDFOption {
	return func(o map[string]interface{}) {
		o["landscape"] = true
	}
}

// PortraitMode instructs PrintToPDF to print pages in portrait mode
func PortraitMode() PrintToPDFOption {
	return func(o map[string]interface{}) {
		o["landscape"] = false
	}
}

// DisplayHeaderFooter instructs PrintToPDF to print headers/footers or not
func DisplayHeaderFooter() PrintToPDFOption {
	return func(o map[string]interface{}) {
		o["displayHeaderFooter"] = true
	}
}

// printBackground instructs PrintToPDF to print background graphics
func PrintBackground() PrintToPDFOption {
	return func(o map[string]interface{}) {
		o["printBackground"] = true
	}
}

// Scale instructs PrintToPDF to scale the pages (1.0 is current scale)
func Scale(n float64) PrintToPDFOption {
	return func(o map[string]interface{}) {
		o["scale"] = n
	}
}

// Dimensions sets the current page dimensions for PrintToPDF
func Dimensions(width, height float64) PrintToPDFOption {
	return func(o map[string]interface{}) {
		o["paperWidth"] = width
		o["paperHeight"] = height
	}
}

// Margins sets the margin sizes for PrintToPDF
func Margins(top, bottom, left, right float64) PrintToPDFOption {
	return func(o map[string]interface{}) {
		o["marginTop"] = top
		o["marginBottom"] = bottom
		o["marginLeft"] = left
		o["marginRight"] = right
	}
}

// PageRanges instructs PrintToPDF to print only the specified range of pages
func PageRanges(ranges string) PrintToPDFOption {
	return func(o map[string]interface{}) {
		o["pageRanges"] = ranges
	}
}

// PrintToPDF print the current page as PDF.
func (remote *RemoteDebugger) PrintToPDF(options ...PrintToPDFOption) ([]byte, error) {
	mOptions := map[string]interface{}{}

	for _, o := range options {
		o(mOptions)
	}

	res, err := remote.SendRequest("Page.printToPDF", mOptions)
	if err != nil {
		return nil, err
	}

	if res == nil {
		return nil, ErrorNoResponse
	}

	return base64.StdEncoding.DecodeString(res["data"].(string))
}

// SavePDF print current page as PDF and save to file
func (remote *RemoteDebugger) SavePDF(filename string, perm os.FileMode, options ...PrintToPDFOption) error {
	rawPDF, err := remote.PrintToPDF(options...)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(filename, rawPDF, perm)
}

// ConsoleAPICallback processes the Runtime.consolAPICalled event and returns printable info
func ConsoleAPICallback(cb func([]interface{})) EventCallback {
	return func(params Params) {
		l := []interface{}{"console." + params["type"].(string)}

		for _, a := range params["args"].([]interface{}) {
			arg := a.(map[string]interface{})

			if arg["value"] != nil {
				l = append(l, arg["value"])
			} else if arg["preview"] != nil {
				arg := arg["preview"].(map[string]interface{})

				v := arg["description"].(string) + "{"

				for i, p := range arg["properties"].([]interface{}) {
					if i > 0 {
						v += ", "
					}

					prop := p.(map[string]interface{})
					if prop["name"] != nil {
						v += fmt.Sprintf("%q: ", prop["name"])
					}

					v += fmt.Sprintf("%v", prop["value"])
				}

				v += "}"
				l = append(l, v)
			} else {
				l = append(l, arg["type"].(string))
			}

		}

		cb(l)
	}
}
