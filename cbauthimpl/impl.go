// @author Couchbase <info@couchbase.com>
// @copyright 2015 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package cbauthimpl contains internal implementation details of
// cbauth. It's APIs are subject to change without notice.
package cbauthimpl

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"
)

// Node struct is used as part of Cache messages to describe creds and
// ports of some cluster node.
type Node struct {
	Host     string
	User     string `json:"admin_user"`
	Password string `json:"admin_pass"`
	Ports    []int
	Local    bool
}

func matchHost(n Node, host string) bool {
	if n.Host == "127.0.0.1" {
		return true
	}
	if host == "127.0.0.1" && n.Local {
		return true
	}
	return host == n.Host
}

func getMemcachedCreds(n Node, host string, port int) (user, password string) {
	if !matchHost(n, host) {
		return "", ""
	}
	for _, p := range n.Ports {
		if p == port {
			return n.User, n.Password
		}
	}
	return "", ""
}

// User struct is used as part of Cache messages to describe creds of
// some user (admin or ro-admin).
type User struct {
	User string
	Salt []byte
	Mac  []byte
}

func verifyCreds(u User, user, password string) bool {
	if u.User == "" || u.User != user {
		return false
	}

	mac := hmac.New(sha1.New, u.Salt)
	mac.Write([]byte(password))
	return hmac.Equal(u.Mac, mac.Sum(nil))
}

type credsDB struct {
	nodes          []Node
	buckets        map[string]string
	admin          User
	roadmin        User
	hasNoPwdBucket bool
	tokenCheckURL  string
}

// Cache is a structure into which the revrpc json is unmarshalled
type Cache struct {
	Nodes         []Node
	Buckets       map[string]string
	Admin         User
	ROAdmin       User   `json:"roAdmin"`
	TokenCheckURL string `json:"tokenCheckUrl"`
}

// CredsImpl implements cbauth.Creds interface.
type CredsImpl struct {
	name      string
	isAdmin   bool
	isROAdmin bool
	password  string
	db        *credsDB
}

func credsFromUserRole(user, role string, db *credsDB) *CredsImpl {
	rv := CredsImpl{name: user, db: db}
	switch role {
	case "admin":
		rv.isAdmin = true
	case "ro_admin":
		rv.isROAdmin = true
	default:
		panic("unknown role: " + role)
	}
	return &rv
}

// Name method returns user name (e.g. for auditing)
func (c *CredsImpl) Name() string {
	return c.name
}

// IsAdmin method returns true iff this creds represent valid
// admin account.
func (c *CredsImpl) IsAdmin() (bool, error) {
	return c.isAdmin, nil
}

// IsROAdmin method returns true iff this creds represent valid
// read only admin account.
func (c *CredsImpl) IsROAdmin() (bool, error) {
	return c.CanReadAnyMetadata(), nil
}

// CanReadAnyMetadata method returns true iff this creds represents
// admin or ro-admin account.
func (c *CredsImpl) CanReadAnyMetadata() bool {
	return c.isROAdmin || c.isAdmin
}

func checkBucketPassword(db *credsDB, bucket, givenPassword string) bool {
	// TODO: one day we'll care enough to do something like
	// subtle.ConstantTimeCompare, but note that it's going to be
	// trickier than just using that function alone. For that
	// reason, I'm keeping away from trouble for now.
	pwd, exists := db.buckets[bucket]
	return exists && pwd == givenPassword
}

// CanAccessBucket method returns true iff this creds
// represent valid account that can read/write/query docs in given
// bucket.
func (c *CredsImpl) CanAccessBucket(bucket string) (bool, error) {
	if c.isAdmin {
		return true, nil
	}
	if c.name != "" && c.name != bucket {
		return false, nil
	}
	return checkBucketPassword(c.db, bucket, c.password), nil
}

// CanReadBucket method returns true iff this creds represent
// valid account that can read (but not necessarily write)
// docs in given bucket.
func (c *CredsImpl) CanReadBucket(bucket string) (bool, error) {
	return c.CanAccessBucket(bucket)
}

// CanDDLBucket method returns true iff this creds represent
// valid account that can DDL in given bucket. Note that at
// this time it delegates to CanAccessBucket in only
// implementation.
func (c *CredsImpl) CanDDLBucket(bucket string) (bool, error) {
	return c.CanAccessBucket(bucket)
}

// Svc is a struct that holds state of cbauth service.
type Svc struct {
	l         sync.Mutex
	db        *credsDB
	freshChan chan struct{}
}

func cacheToCredsDB(c *Cache) *credsDB {
	hasNoPwdBucket := false
	for _, pwd := range c.Buckets {
		if pwd == "" {
			hasNoPwdBucket = true
			break
		}
	}
	return &credsDB{
		nodes:          c.Nodes,
		buckets:        c.Buckets,
		admin:          c.Admin,
		roadmin:        c.ROAdmin,
		hasNoPwdBucket: hasNoPwdBucket,
		tokenCheckURL:  c.TokenCheckURL,
	}
}

// UpdateDB is a revrpc method that is used by ns_server update cbauth
// state.
func (s *Svc) UpdateDB(c *Cache, outparam *bool) error {
	if outparam != nil {
		*outparam = true
	}
	// BUG(alk): consider some kind of CAS later
	db := cacheToCredsDB(c)
	s.l.Lock()
	s.db = db
	if s.freshChan != nil {
		close(s.freshChan)
		s.freshChan = nil
	}
	s.l.Unlock()
	return nil
}

// ResetSvc marks service's db as stale.
func ResetSvc(s *Svc) {
	s.l.Lock()
	s.db = nil
	s.l.Unlock()
}

// NewSVC constructs Svc instance. Period is initial period of time
// where attempts to access stale DB won't cause ErrStale responses,
// but service will instead wait for UpdateDB call.
func NewSVC(period time.Duration) *Svc {
	return NewSVCForTest(period, func(period time.Duration, freshChan chan struct{}, body func()) {
		time.AfterFunc(period, body)
	})
}

// NewSVCForTest constructs Svc isntance.
func NewSVCForTest(period time.Duration, waitfn func(time.Duration, chan struct{}, func())) *Svc {
	s := &Svc{}
	if period != time.Duration(0) {
		s.freshChan = make(chan struct{})
		waitfn(period, s.freshChan, func() {
			s.l.Lock()
			if s.freshChan != nil {
				close(s.freshChan)
				s.freshChan = nil
			}
			s.l.Unlock()
		})
	}
	return s
}

// ErrStale is used to indicate that cbauth state is stale.
var ErrStale = errors.New("CBAuth database is stale")

func fetchDB(s *Svc) *credsDB {
	s.l.Lock()
	db := s.db
	c := s.freshChan
	s.l.Unlock()

	if db != nil || c == nil {
		return db
	}

	// if db is stale try to wait a bit
	<-c
	// double receive doesn't change anything from correctness
	// standpoint (we close channel), but helps a lot for tests
	<-c
	s.l.Lock()
	db = s.db
	s.l.Unlock()

	return db
}

const tokenHeader = "ns_server-ui"

// IsAuthTokenPresent returns true iff ns_server's ui token header
// ("ns_server-ui") is set to "yes". UI is using that header to
// indicate that request is using so called token auth.
func IsAuthTokenPresent(req *http.Request) bool {
	return req.Header.Get(tokenHeader) == "yes"
}

func copyHeader(name string, from, to http.Header) {
	if val := from.Get(name); val != "" {
		to.Set(name, val)
	}
}

// VerifyOnServer verifies auth of given request by passing it to
// ns_server.
func VerifyOnServer(s *Svc, reqHeaders http.Header) (*CredsImpl, error) {
	db := fetchDB(s)
	if db == nil {
		return nil, ErrStale
	}

	if s.db.tokenCheckURL == "" {
		return nil, nil
	}

	req, err := http.NewRequest("POST", db.tokenCheckURL, nil)
	if err != nil {
		panic(err)
	}

	copyHeader(tokenHeader, reqHeaders, req.Header)
	copyHeader("Cookie", reqHeaders, req.Header)
	copyHeader("Authorization", reqHeaders, req.Header)

	hresp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer hresp.Body.Close()
	if hresp.StatusCode == 401 {
		return nil, nil
	}

	if hresp.StatusCode != 200 {
		err = fmt.Errorf("Expecting 200 or 401 from ns_server auth endpoint. Got: %s", hresp.Status)
		return nil, err
	}

	body, err := ioutil.ReadAll(hresp.Body)
	if err != nil {
		return nil, err
	}

	resp := struct {
		Role, User string
	}{}
	err = json.Unmarshal(body, &resp)
	if err != nil {
		return nil, err
	}

	return credsFromUserRole(resp.User, resp.Role, db), nil
}

// VerifyPassword verifies given user/password creds against cbauth
// password database. Returns nil, nil if given creds are not
// recognised at all.
func VerifyPassword(s *Svc, user, password string) (*CredsImpl, error) {
	db := fetchDB(s)
	if db == nil {
		return nil, ErrStale
	}
	rv := &CredsImpl{name: user, password: password, db: db}

	switch {
	case verifyCreds(db.admin, user, password):
		rv.isAdmin = true
	case verifyCreds(db.roadmin, user, password):
		rv.isROAdmin = true
	case user == "":
		if !(password == "" && db.hasNoPwdBucket) {
			// we only allow anonymous access if password
			// is also empty and there is at least one
			// no-password bucket
			return nil, nil
		}
	default:
		if !checkBucketPassword(db, user, password) {
			// right now we only grant access if username
			// matches specific bucket and bucket password
			// is given
			return nil, nil
		}
	}

	return rv, nil
}

// GetPassword returns service password for given host and port. Or
// "", "", nil if host/port represents unknown service.
func GetPassword(s *Svc, host string, port int) (user, pwd string, err error) {
	db := fetchDB(s)
	if db == nil {
		return "", "", ErrStale
	}
	for _, n := range db.nodes {
		user, pwd = getMemcachedCreds(n, host, port)
		if user != "" {
			return
		}
	}
	return
}
