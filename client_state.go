package authboss

import (
	"context"
	"fmt"
	"net/http"
)

const (
	// SessionKey is the primarily used key by authboss.
	SessionKey = "uid"
	// SessionHalfAuthKey is used for sessions that have been authenticated by
	// the remember module. This serves as a way to force full authentication
	// by denying half-authed users acccess to sensitive areas.
	SessionHalfAuthKey = "halfauth"
	// SessionLastAction is the session key to retrieve the last action of a user.
	SessionLastAction = "last_action"
	// SessionOAuth2State is the xsrf protection key for oauth.
	SessionOAuth2State = "oauth2_state"
	// SessionOAuth2Params is the additional settings for oauth like redirection/remember.
	SessionOAuth2Params = "oauth2_params"

	// CookieRemember is used for cookies and form input names.
	CookieRemember = "rm"

	// FlashSuccessKey is used for storing sucess flash messages on the session
	FlashSuccessKey = "flash_success"
	// FlashErrorKey is used for storing sucess flash messages on the session
	FlashErrorKey = "flash_error"
)

// ClientStateEventKind is an enum.
type ClientStateEventKind int

// ClientStateEvent kinds
const (
	ClientStateEventPut ClientStateEventKind = iota
	ClientStateEventDel
)

// ClientStateEvent are the different events that can be recorded during
type ClientStateEvent struct {
	Kind  ClientStateEventKind
	Key   string
	Value string
}

// ClientStateReadWriter is used to create a cookie storer from an http request.
// Keep in mind security considerations for your implementation, Secure,
// HTTP-Only, etc flags.
//
// There's two major uses for this. To create session storage, and remember me
// cookies.
type ClientStateReadWriter interface {
	// ReadState should return a map like structure allowing it to look up
	// any values in the current session, or any cookie in the request
	ReadState(http.ResponseWriter, *http.Request) (ClientState, error)
	// WriteState can sometimes be called with a nil ClientState in the event
	// that no ClientState was recovered from the request context.
	WriteState(http.ResponseWriter, ClientState, []ClientStateEvent) error
}

// UnderlyingResponseWriter retrieves the response
// writer underneath the current one. This allows us
// to wrap and later discover the particular one that we want.
// Keep in mind this should not be used to call the normal methods
// of a responsewriter, just additional ones particular to that type
// because it's possible to introduce subtle bugs otherwise.
type UnderlyingResponseWriter interface {
	UnderlyingResponseWriter() http.ResponseWriter
}

// ClientState represents the client's current state and can answer queries
// about it.
type ClientState interface {
	Get(key string) (string, bool)
}

// ClientStateResponseWriter is used to write out the client state at the last
// moment before the response code is written.
type ClientStateResponseWriter struct {
	http.ResponseWriter

	cookieState  ClientStateReadWriter
	sessionState ClientStateReadWriter

	hasWritten         bool
	ctx                context.Context
	sessionStateEvents []ClientStateEvent
	cookieStateEvents  []ClientStateEvent
}

// LoadClientStateMiddleware wraps all requests with the ClientStateResponseWriter
// as well as loading the current client state into the context for use.
func (a *Authboss) LoadClientStateMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, err := a.LoadClientState(w, r)
		if err != nil {
			panic(fmt.Sprintf("failed to load client state: %+v", err))
		}

		writer := a.NewResponse(w, request)

		h.ServeHTTP(writer, request)
	})
}

// NewResponse wraps the ResponseWriter with a ClientStateResponseWriter
func (a *Authboss) NewResponse(w http.ResponseWriter, r *http.Request) *ClientStateResponseWriter {
	return &ClientStateResponseWriter{
		ResponseWriter: w,
		cookieState:    a.Config.Storage.CookieState,
		sessionState:   a.Config.Storage.SessionState,
		ctx:            r.Context(),
	}
}

// LoadClientState loads the state from sessions and cookies into the request context
func (a *Authboss) LoadClientState(w http.ResponseWriter, r *http.Request) (*http.Request, error) {
	if a.Storage.SessionState != nil {
		state, err := a.Storage.SessionState.ReadState(w, r)
		if err != nil {
			return nil, err
		} else if state == nil {
			return r, nil
		}

		ctx := context.WithValue(r.Context(), CTXKeySessionState, state)
		r = r.WithContext(ctx)
	}
	if a.Storage.CookieState != nil {
		state, err := a.Storage.CookieState.ReadState(w, r)
		if err != nil {
			return nil, err
		} else if state == nil {
			return r, nil
		}
		ctx := context.WithValue(r.Context(), CTXKeyCookieState, state)
		r = r.WithContext(ctx)
	}

	return r, nil
}

// MustClientStateResponseWriter tries to find a csrw inside the response
// writer by using the UnderlyingResponseWriter interface.
func MustClientStateResponseWriter(w http.ResponseWriter) *ClientStateResponseWriter {
	for {
		if c, ok := w.(*ClientStateResponseWriter); ok {
			return c
		}

		if u, ok := w.(UnderlyingResponseWriter); ok {
			w = u.UnderlyingResponseWriter()
			continue
		}

		panic(fmt.Sprintf("failed to find a ClientStateResponseWriter or UnderlyingResponseWriter in: %T", w))
	}
}

// WriteHeader writes the header, but in order to handle errors from the
// underlying ClientStateReadWriter, it has to panic.
func (c *ClientStateResponseWriter) WriteHeader(code int) {
	if !c.hasWritten {
		if err := c.putClientState(); err != nil {
			panic(err)
		}
	}
	c.ResponseWriter.WriteHeader(code)
}

// Header retrieves the underlying headers
func (c ClientStateResponseWriter) Header() http.Header {
	return c.ResponseWriter.Header()
}

// Write ensures that the client state is written before any writes
// to the body occur (before header flush to http client)
func (c *ClientStateResponseWriter) Write(b []byte) (int, error) {
	if !c.hasWritten {
		if err := c.putClientState(); err != nil {
			return 0, err
		}
	}
	return c.ResponseWriter.Write(b)
}

// UnderlyingResponseWriter for this instance
func (c *ClientStateResponseWriter) UnderlyingResponseWriter() http.ResponseWriter {
	return c.ResponseWriter
}

func (c *ClientStateResponseWriter) putClientState() error {
	if c.hasWritten {
		panic("should not call putClientState twice")
	}
	c.hasWritten = true

	if len(c.cookieStateEvents) == 0 && len(c.sessionStateEvents) == 0 {
		return nil
	}

	if c.sessionState != nil && len(c.sessionStateEvents) > 0 {
		sessionStateIntf := c.ctx.Value(CTXKeySessionState)

		var session ClientState
		if sessionStateIntf != nil {
			session = sessionStateIntf.(ClientState)
		}

		err := c.sessionState.WriteState(c, session, c.sessionStateEvents)
		if err != nil {
			return err
		}
	}
	if c.cookieState != nil && len(c.cookieStateEvents) > 0 {
		cookieStateIntf := c.ctx.Value(CTXKeyCookieState)

		var cookie ClientState
		if cookieStateIntf != nil {
			cookie = cookieStateIntf.(ClientState)
		}

		err := c.cookieState.WriteState(c, cookie, c.cookieStateEvents)
		if err != nil {
			return err
		}
	}

	return nil
}

// PutSession puts a value into the session
func PutSession(w http.ResponseWriter, key, val string) {
	putState(w, CTXKeySessionState, key, val)
}

// DelSession deletes a key-value from the session.
func DelSession(w http.ResponseWriter, key string) {
	delState(w, CTXKeySessionState, key)
}

// GetSession fetches a value from the session
func GetSession(r *http.Request, key string) (string, bool) {
	return getState(r, CTXKeySessionState, key)
}

// PutCookie puts a value into the session
func PutCookie(w http.ResponseWriter, key, val string) {
	putState(w, CTXKeyCookieState, key, val)
}

// DelCookie deletes a key-value from the session.
func DelCookie(w http.ResponseWriter, key string) {
	delState(w, CTXKeyCookieState, key)
}

// GetCookie fetches a value from the session
func GetCookie(r *http.Request, key string) (string, bool) {
	return getState(r, CTXKeyCookieState, key)
}

func putState(w http.ResponseWriter, CTXKey contextKey, key, val string) {
	setState(w, CTXKey, ClientStateEventPut, key, val)
}

func delState(w http.ResponseWriter, CTXKey contextKey, key string) {
	setState(w, CTXKey, ClientStateEventDel, key, "")
}

func setState(w http.ResponseWriter, CTXKey contextKey, op ClientStateEventKind, key, val string) {
	csrw := MustClientStateResponseWriter(w)
	ev := ClientStateEvent{
		Kind: op,
		Key:  key,
	}

	if op == ClientStateEventPut {
		ev.Value = val
	}

	switch CTXKey {
	case CTXKeySessionState:
		csrw.sessionStateEvents = append(csrw.sessionStateEvents, ev)
	case CTXKeyCookieState:
		csrw.cookieStateEvents = append(csrw.cookieStateEvents, ev)
	}
}

func getState(r *http.Request, CTXKey contextKey, key string) (string, bool) {
	val := r.Context().Value(CTXKey)
	if val == nil {
		return "", false
	}

	state := val.(ClientState)
	return state.Get(key)
}

// FlashSuccess returns FlashSuccessKey from the session and removes it.
func FlashSuccess(w http.ResponseWriter, r *http.Request) string {
	str, ok := GetSession(r, FlashSuccessKey)
	if !ok {
		return ""
	}

	DelSession(w, FlashSuccessKey)
	return str
}

// FlashError returns FlashError from the session and removes it.
func FlashError(w http.ResponseWriter, r *http.Request) string {
	str, ok := GetSession(r, FlashErrorKey)
	if !ok {
		return ""
	}

	DelSession(w, FlashErrorKey)
	return str
}