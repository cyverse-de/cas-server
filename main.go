package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"

	"github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/pkg/errors"
)

var log = logrus.WithFields(logrus.Fields{
	"service": "cas-proxy",
	"art-id":  "cas-proxy",
	"group":   "org.cyverse",
})

func init() {
	logrus.SetFormatter(&logrus.JSONFormatter{})
}

const sessionName = "proxy-session"
const sessionKey = "proxy-session-key"

// CASProxy contains the application logic that handles authentication, session
// validations, ticket validation, and request proxying.
type CASProxy struct {
	casBase     string // base URL for the CAS server
	casValidate string // The path to the validation endpoint on the CAS server.
	frontendURL string // The URL placed into service query param for CAS.
	backendURL  string // The backend URL to forward to.
	cookies     *sessions.CookieStore
}

// NewCASProxy returns a newly instantiated *CASProxy.
func NewCASProxy(casBase, casValidate, frontendURL, backendURL string) *CASProxy {
	return &CASProxy{
		casBase:     casBase,
		casValidate: casValidate,
		frontendURL: frontendURL,
		backendURL:  backendURL,
		cookies:     sessions.NewCookieStore([]byte("omgsekretz")), // TODO: replace
	}
}

// ValidateTicket will validate a CAS ticket against the configured CAS server.
func (c *CASProxy) ValidateTicket(w http.ResponseWriter, r *http.Request) {
	casURL, err := url.Parse(c.casBase)
	if err != nil {
		err = errors.Wrapf(err, "failed to parse CAS base URL %s", c.casBase)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Make sure the path in the CAS params is the same as the one that was
	// requested.
	svcURL, err := url.Parse(c.frontendURL)
	if err != nil {
		err = errors.Wrapf(err, "failed to parse the frontend URL %s", c.frontendURL)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Make sure the serivce path and the query params are set to the incoming
	// requests values for those fields.
	svcURL.Path = r.URL.Path
	sq := r.URL.Query()
	sq.Del("ticket")
	svcURL.RawQuery = sq.Encode()

	casURL.Path = path.Join(casURL.Path, c.casValidate)
	q := casURL.Query()
	q.Add("service", svcURL.String())
	q.Add("ticket", r.URL.Query().Get("ticket"))
	casURL.RawQuery = q.Encode()

	resp, err := http.Get(casURL.String())
	if err != nil {
		err = errors.Wrap(err, "ticket validation error")
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err = errors.Wrapf(err, "ticket validation status code was %d", resp.StatusCode)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		err = errors.Wrap(err, "error reading body of CAS response")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if bytes.Equal(b, []byte("no\n\n")) {
		err = fmt.Errorf("ticket validation response body was %s", b)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	//Store a session, hopefully to short circuit the CAS redirect dance in later
	//requests.
	session, err := c.cookies.Get(r, sessionName)
	if err != nil {
		err = errors.Wrapf(err, "failed get session %s", sessionName)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	session.Values[sessionKey] = 1
	c.cookies.Save(r, w, session)

	http.Redirect(w, r, svcURL.String(), http.StatusTemporaryRedirect)
}

// Session implements the mux.Matcher interface so that requests can be routed
// based on cookie existence.
func (c *CASProxy) Session(r *http.Request, m *mux.RouteMatch) bool {
	var (
		val interface{}
		ok  bool
	)
	session, err := c.cookies.Get(r, sessionName)
	if err != nil {
		return true
	}
	if val, ok = session.Values[sessionKey]; !ok {
		log.Infof("key %s was not in the session", sessionKey)
		return true
	}
	if val.(int) != 1 {
		log.Infof("session value was %d instead of 1", val.(int))
		return true
	}
	return false
}

// RedirectToCAS redirects the request to CAS, setting the service query
// parameter to the value in frontendURL.
func (c *CASProxy) RedirectToCAS(w http.ResponseWriter, r *http.Request) {
	casURL, err := url.Parse(c.casBase)
	if err != nil {
		err = errors.Wrapf(err, "failed to parse CAS base URL %s", c.casBase)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Make sure the path in the CAS params is the same as the one that was
	// requested.
	svcURL, err := url.Parse(c.frontendURL)
	if err != nil {
		err = errors.Wrapf(err, "failed to parse the frontend URL %s", c.frontendURL)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Make sure the serivce path and the query params are set to the incoming
	// requests values for those fields.
	svcURL.Path = r.URL.Path
	svcURL.RawQuery = r.URL.RawQuery

	//set the service query param in the casURL.
	q := casURL.Query()
	q.Add("service", svcURL.String())
	casURL.RawQuery = q.Encode()
	casURL.Path = path.Join(casURL.Path, "login")

	// perform the redirect
	http.Redirect(w, r, casURL.String(), http.StatusTemporaryRedirect)
}

func main() {
	var (
		backendURL  = flag.String("backend-url", "http://localhost:60000", "The hostname and port to proxy requests to.")
		frontendURL = flag.String("frontend-url", "", "The URL for the frontend server. Might be different from the hostname and listen port.")
		listenAddr  = flag.String("listen-addr", "0.0.0.0:8080", "The listen port number.")
		casBase     = flag.String("cas-base-url", "", "The base URL to the CAS host.")
		casValidate = flag.String("cas-validate", "validate", "The CAS URL endpoint for validating tickets.")
	)

	flag.Parse()

	if *casBase == "" {
		log.Fatal("--cas-base-url must be set.")
	}

	if *frontendURL == "" {
		log.Fatal("--frontend-url must be set.")
	}

	log.Infof("backend URL is %s", *backendURL)
	log.Infof("frontend URL is %s", *frontendURL)
	log.Infof("listen address is %s", *listenAddr)
	log.Infof("CAS base URL is %s", *casBase)
	log.Infof("CAS ticket validator endpoint is %s", *casValidate)

	p := NewCASProxy(*casBase, *casValidate, *frontendURL, *backendURL)

	r := mux.NewRouter()

	// If the query containes a ticket in the query params, then it needs to be
	// validated.
	r.PathPrefix("/").Queries("ticket", "").Handler(http.HandlerFunc(p.ValidateTicket))
	r.PathPrefix("/").MatcherFunc(p.Session).Handler(http.HandlerFunc(p.RedirectToCAS))
	r.PathPrefix("/").Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range r.Header {
			fmt.Fprintf(w, "key: %s, value: %s    \n", k, v)
		}
		fmt.Fprintln(w, "test successful")
	}))

	server := &http.Server{
		Handler: r,
		Addr:    *listenAddr,
	}
	log.Fatal(server.ListenAndServe())

}
