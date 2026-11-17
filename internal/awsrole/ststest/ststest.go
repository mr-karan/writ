// Package ststest is a stand-in for AWS STS in tests. It answers
// AssumeRole and GetCallerIdentity in the query protocol the AWS SDKs
// speak, records every request with the access key id that signed it, and
// issues credentials that only it knows. It checks no signature.
package ststest

import (
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request is one request the server received.
type Request struct {
	Action string
	Form   url.Values
	// AccessKeyID is the key id in the SigV4 Authorization header.
	AccessKeyID string
	// Region and Service come from the same header's credential scope.
	Region, Service string
	// SecurityToken is the X-Amz-Security-Token header.
	SecurityToken string
}

// Issued is one set of credentials the server issued.
type Issued struct {
	AccessKeyID, SecretAccessKey, SessionToken string
	Expires                                    time.Time
	Role, SessionName, SourceIdentity          string
	Duration                                   time.Duration
}

// Server is a running stand-in.
type Server struct {
	URL string
	srv *httptest.Server

	mu       sync.Mutex
	requests []Request
	issued   map[string]Issued
	deny     map[string]bool
	static   map[string][2]string
	n        int
	// lifetime, when set, replaces the requested duration of the
	// credentials it issues, so a test can see them expire.
	lifetime time.Duration
}

// New starts a server on a loopback address.
func New() *Server {
	s := &Server{issued: map[string]Issued{}, deny: map[string]bool{}, static: map[string][2]string{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	return s
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// Deny makes AssumeRole of role fail with AccessDenied.
func (s *Server) Deny(role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deny[role] = true
}

// Static makes AssumeRole of role issue the access key id akid and the
// secret key secret with no session token, as keys of a user that a local
// S3 stand-in knows, which takes no session token.
func (s *Server) Static(role, akid, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.static[role] = [2]string{akid, secret}
}

// SetLifetime makes the credentials issued from now on last d.
func (s *Server) SetLifetime(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifetime = d
}

// Requests returns every request so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Issued returns the credentials with access key id akid.
func (s *Server) Issued(akid string) (Issued, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.issued[akid]
	return i, ok
}

var credentialRE = regexp.MustCompile(`Credential=([^/]+)/[0-9]{8}/([^/]+)/([^/]+)/aws4_request`)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	form, err := url.ParseQuery(string(body))
	if err != nil || r.Method != http.MethodPost {
		writeError(w, http.StatusBadRequest, "InvalidAction", "post a form")
		return
	}
	req := Request{Action: form.Get("Action"), Form: form, SecurityToken: r.Header.Get("X-Amz-Security-Token")}
	if m := credentialRE.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		req.AccessKeyID, req.Region, req.Service = m[1], m[2], m[3]
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	switch req.Action {
	case "AssumeRole":
		s.assumeRole(w, req)
	case "GetCallerIdentity":
		s.callerIdentity(w, req)
	default:
		writeError(w, http.StatusBadRequest, "InvalidAction", "ststest answers AssumeRole and GetCallerIdentity")
	}
}

func (s *Server) assumeRole(w http.ResponseWriter, req Request) {
	role, name := req.Form.Get("RoleArn"), req.Form.Get("RoleSessionName")
	secs, _ := strconv.Atoi(req.Form.Get("DurationSeconds"))
	s.mu.Lock()
	denied := s.deny[role]
	life := time.Duration(secs) * time.Second
	if s.lifetime > 0 {
		life = s.lifetime
	}
	s.n++
	i := Issued{
		AccessKeyID: fmt.Sprintf("ASIATEST%012d", s.n), SecretAccessKey: rand.Text(), SessionToken: rand.Text(),
		Expires: time.Now().Add(life).UTC().Truncate(time.Second), Role: role, SessionName: name,
		SourceIdentity: req.Form.Get("SourceIdentity"), Duration: time.Duration(secs) * time.Second,
	}
	if keys, ok := s.static[role]; ok {
		i.AccessKeyID, i.SecretAccessKey, i.SessionToken = keys[0], keys[1], ""
	}
	if !denied {
		s.issued[i.AccessKeyID] = i
	}
	s.mu.Unlock()
	if denied {
		writeError(w, http.StatusForbidden, "AccessDenied", "User: control-plane is not authorized to perform: sts:AssumeRole on resource: "+role)
		return
	}
	account, roleName := accountOf(role), role[strings.LastIndex(role, "/")+1:]
	type result struct {
		XMLName         xml.Name `xml:"https://sts.amazonaws.com/doc/2011-06-15/ AssumeRoleResponse"`
		SourceIdentity  string   `xml:"AssumeRoleResult>SourceIdentity,omitempty"`
		Arn             string   `xml:"AssumeRoleResult>AssumedRoleUser>Arn"`
		AssumedRoleID   string   `xml:"AssumeRoleResult>AssumedRoleUser>AssumedRoleId"`
		AccessKeyID     string   `xml:"AssumeRoleResult>Credentials>AccessKeyId"`
		SecretAccessKey string   `xml:"AssumeRoleResult>Credentials>SecretAccessKey"`
		SessionToken    string   `xml:"AssumeRoleResult>Credentials>SessionToken"`
		Expiration      string   `xml:"AssumeRoleResult>Credentials>Expiration"`
		RequestID       string   `xml:"ResponseMetadata>RequestId"`
	}
	writeXML(w, result{
		SourceIdentity: i.SourceIdentity, Arn: "arn:aws:sts::" + account + ":assumed-role/" + roleName + "/" + name,
		AssumedRoleID: "AROATEST:" + name, AccessKeyID: i.AccessKeyID, SecretAccessKey: i.SecretAccessKey,
		SessionToken: i.SessionToken, Expiration: i.Expires.Format(time.RFC3339), RequestID: rand.Text(),
	})
}

func (s *Server) callerIdentity(w http.ResponseWriter, req Request) {
	i, ok := s.Issued(req.AccessKeyID)
	if !ok || i.SessionToken != req.SecurityToken {
		writeError(w, http.StatusForbidden, "InvalidClientTokenId", "The security token included in the request is invalid.")
		return
	}
	if time.Now().After(i.Expires) {
		writeError(w, http.StatusForbidden, "ExpiredToken", "The security token included in the request is expired")
		return
	}
	account, roleName := accountOf(i.Role), i.Role[strings.LastIndex(i.Role, "/")+1:]
	type result struct {
		XMLName   xml.Name `xml:"https://sts.amazonaws.com/doc/2011-06-15/ GetCallerIdentityResponse"`
		Arn       string   `xml:"GetCallerIdentityResult>Arn"`
		UserID    string   `xml:"GetCallerIdentityResult>UserId"`
		Account   string   `xml:"GetCallerIdentityResult>Account"`
		RequestID string   `xml:"ResponseMetadata>RequestId"`
	}
	writeXML(w, result{Arn: "arn:aws:sts::" + account + ":assumed-role/" + roleName + "/" + i.SessionName, UserID: "AROATEST:" + i.SessionName, Account: account, RequestID: rand.Text()})
}

func accountOf(role string) string {
	f := strings.Split(role, ":")
	if len(f) < 5 {
		return ""
	}
	return f[4]
}

func writeXML(w http.ResponseWriter, v any) {
	b, err := xml.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalFailure", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write(append([]byte(xml.Header), b...))
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	type errorResponse struct {
		XMLName   xml.Name `xml:"https://sts.amazonaws.com/doc/2011-06-15/ ErrorResponse"`
		Type      string   `xml:"Error>Type"`
		Code      string   `xml:"Error>Code"`
		Message   string   `xml:"Error>Message"`
		RequestID string   `xml:"RequestId"`
	}
	b, _ := xml.Marshal(errorResponse{Type: "Sender", Code: code, Message: msg, RequestID: rand.Text()})
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = w.Write(append([]byte(xml.Header), b...))
}
