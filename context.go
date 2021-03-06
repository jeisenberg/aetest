// Copyright 2013 Google Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

/*
Package aetest provides an appengine.Context for use in tests.

An example test file:

	package foo_test

	import (
		"testing"

		"appengine/memcache"
		"appengine/aetest"
	)

	func TestFoo(t *testing.T) {
		c, err := aetest.NewContext(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()

		it := &memcache.Item{
			Key:   "some-key",
			Value: []byte("some-value"),
		}
		err = memcache.Set(c, it)
		if err != nil {
			t.Fatalf("Set err: %v", err)
		}
		it, err = memcache.Get(c, "some-key")
		if err != nil {
			t.Fatalf("Get err: %v; want no error", err)
		}
		if g, w := string(it.Value), "some-value" ; g != w {
			t.Errorf("retrieved Item.Value = %q, want %q", g, w)
		}
	}

The environment variable APPENGINE_DEV_APPSERVER specifies the location of the
dev_appserver.py executable to use. If unset, the system PATH is consulted.
*/
package aetest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"appengine"
	user "appengine/user"
	"appengine_internal"
	"code.google.com/p/goprotobuf/proto"

	basepb "appengine_internal/base"
	remoteapipb "appengine_internal/remote_api"
)

// Context is an appengine.Context that sends all App Engine API calls to an
// instance of the API server.
type Context interface {
	appengine.Context

	// Login causes the context to act as the given user.
	Login(*user.User)
	// Logout causes the context to act as a logged-out user.
	Logout()
	// Close kills the child api_server.py process,
	// releasing its resources.
	io.Closer
}

func btos(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// NewContext launches an instance of api_server.py and returns a Context
// that delegates all App Engine API calls to that instance.
// If opts is nil the default values are used.
func NewContext(opts *Options) (Context, error) {
	req, _ := http.NewRequest("GET", "/", nil)
	c := &context{
		appID:   opts.appID(),
		req:     req,
		session: newSessionID(),
	}
	if err := c.startChild(); err != nil {
		return nil, err
	}
	return c, nil
}

func newSessionID() string {
	var buf [16]byte
	io.ReadFull(rand.Reader, buf[:])
	return fmt.Sprintf("%x", buf[:])
}

// TODO: option to pass flags to api_server.py

// Options is used to specify options when creating a Context.
type Options struct {
	// AppID specifies the App ID to use during tests.
	// By default, "testapp".
	AppID string
}

func (o *Options) appID() string {
	if o == nil || o.AppID == "" {
		return "testapp"
	}
	return o.AppID
}

// PrepareDevAppserver is a hook which, if set, will be called before the
// dev_appserver.py is started, each time it is started. If aetest.NewContext
// is invoked from the goapp test tool, this hook is unnecessary.
var PrepareDevAppserver func() error

// context implements appengine.Context by running an api_server.py
// process as a child and proxying all Context calls to the child.
type context struct {
	appID    string
	req      *http.Request
	child    *exec.Cmd
	apiURL   string // base URL of API HTTP server
	adminURL string // base URL of admin HTTP server
	appDir   string
	session  string
}

func (c *context) AppID() string               { return c.appID }
func (c *context) Request() interface{}        { return c.req }
func (c *context) FullyQualifiedAppID() string { return "dev~" + c.appID }

func (c *context) logf(level, format string, args ...interface{}) {
	log.Printf(level+": "+format, args...)
}

func (c *context) Debugf(format string, args ...interface{})    { c.logf("DEBUG", format, args...) }
func (c *context) Infof(format string, args ...interface{})     { c.logf("INFO", format, args...) }
func (c *context) Warningf(format string, args ...interface{})  { c.logf("WARNING", format, args...) }
func (c *context) Errorf(format string, args ...interface{})    { c.logf("ERROR", format, args...) }
func (c *context) Criticalf(format string, args ...interface{}) { c.logf("CRITICAL", format, args...) }

var errTimeout = &appengine_internal.CallError{
	Detail:  "Deadline exceeded",
	Code:    11, // CANCELED
	Timeout: true,
}

// postWithTimeout issues a POST to the specified URL with a given timeout.
func postWithTimeout(url, bodyType string, body io.Reader, timeout time.Duration) (b []byte, err error) {
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", bodyType)
	tr := &http.Transport{}
	client := &http.Client{
		Transport: tr,
	}
	if timeout != 0 {
		var canceled int32 // atomic; set to 1 if canceled
		t := time.AfterFunc(timeout, func() {
			atomic.StoreInt32(&canceled, 1)
			tr.CancelRequest(req)
		})
		defer t.Stop()
		defer func() {
			// Check to see whether the call was canceled.
			if atomic.LoadInt32(&canceled) != 0 {
				err = errTimeout
			}
		}()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return ioutil.ReadAll(resp.Body)
}

func call(service, method string, data []byte, apiAddress, requestID string, timeout time.Duration) ([]byte, error) {
	req := &remoteapipb.Request{
		ServiceName: proto.String(service),
		Method:      proto.String(method),
		Request:     data,
		RequestId:   proto.String(requestID),
	}

	buf, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}

	body, err := postWithTimeout(apiAddress, "application/octet-stream", bytes.NewReader(buf), timeout)
	if err != nil {
		return nil, err
	}

	res := &remoteapipb.Response{}
	err = proto.Unmarshal(body, res)
	if err != nil {
		return nil, err
	}

	if ae := res.ApplicationError; ae != nil {
		// All Remote API application errors are API-level failures.
		return nil, &appengine_internal.APIError{Service: service, Detail: *ae.Detail, Code: *ae.Code}
	}
	return res.Response, nil
}

// Call is an implementation of appengine.Context's Call that delegates
// to a child api_server.py instance.
func (c *context) Call(service, method string, in, out appengine_internal.ProtoMessage, opts *appengine_internal.CallOptions) error {
	if service == "__go__" && (method == "GetNamespace" || method == "GetDefaultNamespace") {
		out.(*basepb.StringProto).Value = proto.String("")
		return nil
	}
	data, err := proto.Marshal(in)
	if err != nil {
		return err
	}
	var d time.Duration
	if opts != nil && opts.Timeout != 0 {
		d = opts.Timeout
	}
	res, err := call(service, method, data, c.apiURL, c.session, d)
	if err != nil {
		return err
	}
	return proto.Unmarshal(res, out)
}

// Close kills the child api_server.py process, releasing its resources.
// Close is not part of the appengine.Context interface.
func (c *context) Close() (err error) {
	if c.child == nil {
		return nil
	}
	defer func() {
		c.child = nil
		err1 := os.RemoveAll(c.appDir)
		if err == nil {
			err = err1
		}
	}()

	if p := c.child.Process; p != nil {
		errc := make(chan error, 1)
		go func() {
			errc <- c.child.Wait()
		}()

		// Call the quit handler on the admin server.
		res, err := http.Get(c.adminURL + "/quit")
		if err != nil {
			p.Kill()
			return fmt.Errorf("unable to call /quit handler: %v", err)
		}
		res.Body.Close()

		select {
		case <-time.After(15 * time.Second):
			p.Kill()
			return errors.New("timeout killing child process")
		case err = <-errc:
			// Do nothing.
		}
	}
	return
}

func (c *context) Login(u *user.User) {
	c.req.Header.Set("X-AppEngine-User-Email", u.Email)
	id := u.ID
	if id == "" {
		id = strconv.Itoa(int(crc32.Checksum([]byte(u.Email), crc32.IEEETable)))
	}
	c.req.Header.Set("X-AppEngine-User-Id", id)
	c.req.Header.Set("X-AppEngine-User-Federated-Identity", u.Email)
	c.req.Header.Set("X-AppEngine-User-Federated-Provider", u.FederatedProvider)
	c.req.Header.Set("X-AppEngine-User-Is-Admin", btos(u.Admin))
}

func (c *context) Logout() {
	c.req.Header.Del("X-AppEngine-User-Email")
	c.req.Header.Del("X-AppEngine-User-Id")
	c.req.Header.Del("X-AppEngine-User-Is-Admin")
	c.req.Header.Del("X-AppEngine-User-Federated-Identity")
	c.req.Header.Del("X-AppEngine-User-Federated-Provider")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func findPython() (path string, err error) {
	for _, name := range []string{"python2.7", "python"} {
		path, err = exec.LookPath(name)
		if err == nil {
			return
		}
	}
	return
}

func findDevAppserver() (string, error) {
	if p := os.Getenv("APPENGINE_DEV_APPSERVER"); p != "" {
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("invalid APPENGINE_DEV_APPSERVER environment variable; path %q doesn't exist", p)
	}
	return exec.LookPath("dev_appserver.py")
}

var apiServerAddrRE = regexp.MustCompile(`Starting API server at: (\S+)`)
var adminServerAddrRE = regexp.MustCompile(`Starting admin server at: (\S+)`)

func (c *context) startChild() (err error) {
	if PrepareDevAppserver != nil {
		if err := PrepareDevAppserver(); err != nil {
			return err
		}
	}
	python, err := findPython()
	if err != nil {
		return fmt.Errorf("Could not find python interpreter: %v", err)
	}
	devAppserver, err := findDevAppserver()
	if err != nil {
		return fmt.Errorf("Could not find dev_appserver.py: %v", err)
	}

	c.appDir, err = ioutil.TempDir("", "appengine-aetest")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(c.appDir)
		}
	}()
	err = ioutil.WriteFile(filepath.Join(c.appDir, "app.yaml"), []byte(c.appYAML()), 0644)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filepath.Join(c.appDir, "stubapp.go"), []byte(appSource), 0644)
	if err != nil {
		return err
	}

	c.child = exec.Command(
		python,
		devAppserver,
		"--port=0",
		"--api_port=0",
		"--admin_port=0",
		"--skip_sdk_update_check=true",
		"--clear_datastore=true",
		"--datastore_consistency_policy=consistent",
		c.appDir,
	)
	c.child.Stdout = os.Stdout
	var stderr io.Reader
	stderr, err = c.child.StderrPipe()
	if err != nil {
		return err
	}
	stderr = io.TeeReader(stderr, os.Stderr)
	if err = c.child.Start(); err != nil {
		return err
	}

	// Wait until we have read the URLs of the API server and admin interface.
	errc := make(chan error, 1)
	apic := make(chan string)
	adminc := make(chan string)
	go func() {
		s := bufio.NewScanner(stderr)
		for s.Scan() {
			if match := apiServerAddrRE.FindSubmatch(s.Bytes()); match != nil {
				apic <- string(match[1])
			}
			if match := adminServerAddrRE.FindSubmatch(s.Bytes()); match != nil {
				adminc <- string(match[1])
			}
		}
		if err = s.Err(); err != nil {
			errc <- err
		}
	}()

	for c.apiURL == "" || c.adminURL == "" {
		select {
		case c.apiURL = <-apic:
		case c.adminURL = <-adminc:
		case <-time.After(15 * time.Second):
			if p := c.child.Process; p != nil {
				p.Kill()
			}
			return errors.New("timeout starting child process")
		case err := <-errc:
			return fmt.Errorf("error reading child process stderr: %v", err)
		}
	}
	return nil
}

func (c *context) appYAML() string {
	return fmt.Sprintf(appYAMLTemplate, c.appID)
}

const appYAMLTemplate = `
application: %s
version: 1
runtime: go
api_version: go1

handlers:
- url: /.*
  script: _go_app
`

const appSource = `
package nihilist

func init() {}
`
