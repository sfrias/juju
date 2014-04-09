// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"code.google.com/p/go.net/websocket"
	"github.com/juju/loggo"
	jc "github.com/juju/testing/checkers"
	gc "launchpad.net/gocheck"

	"launchpad.net/juju-core/charm"
	jujutesting "launchpad.net/juju-core/juju/testing"
	"launchpad.net/juju-core/state/api"
	"launchpad.net/juju-core/state/api/params"
	"launchpad.net/juju-core/testing"
)

type clientSuite struct {
	jujutesting.JujuConnSuite
}

var _ = gc.Suite(&clientSuite{})

// TODO(jam) 2013-08-27 http://pad.lv/1217282
// Right now most of the direct tests for api.Client behavior are in
// state/apiserver/client/*_test.go

func (s *clientSuite) TestCloseMultipleOk(c *gc.C) {
	client := s.APIState.Client()
	c.Assert(client.Close(), gc.IsNil)
	c.Assert(client.Close(), gc.IsNil)
	c.Assert(client.Close(), gc.IsNil)
}

func (s *clientSuite) TestAddLocalCharm(c *gc.C) {
	charmArchive := testing.Charms.Bundle(c.MkDir(), "dummy")
	curl := charm.MustParseURL(
		fmt.Sprintf("local:quantal/%s-%d", charmArchive.Meta().Name, charmArchive.Revision()),
	)
	client := s.APIState.Client()

	// Test the sanity checks first.
	_, err := client.AddLocalCharm(charm.MustParseURL("cs:quantal/wordpress-1"), nil)
	c.Assert(err, gc.ErrorMatches, `expected charm URL with local: schema, got "cs:quantal/wordpress-1"`)

	// Upload an archive with its original revision.
	savedURL, err := client.AddLocalCharm(curl, charmArchive)
	c.Assert(err, gc.IsNil)
	c.Assert(savedURL.String(), gc.Equals, curl.String())

	// Upload a charm directory with changed revision.
	charmDir := testing.Charms.ClonedDir(c.MkDir(), "dummy")
	charmDir.SetDiskRevision(42)
	savedURL, err = client.AddLocalCharm(curl, charmDir)
	c.Assert(err, gc.IsNil)
	c.Assert(savedURL.Revision, gc.Equals, 42)

	// Upload a charm directory again, revision should be bumped.
	savedURL, err = client.AddLocalCharm(curl, charmDir)
	c.Assert(err, gc.IsNil)
	c.Assert(savedURL.String(), gc.Equals, curl.WithRevision(43).String())

	// Finally, try the NotImplementedError by mocking the server
	// address to a handler that returns 405 Method Not Allowed for
	// POST.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	c.Assert(err, gc.IsNil)
	defer lis.Close()
	url := fmt.Sprintf("http://%v", lis.Addr())
	http.HandleFunc("/charms", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})
	go func() {
		http.Serve(lis, nil)
	}()

	api.SetServerRoot(client, url)
	_, err = client.AddLocalCharm(curl, charmArchive)
	c.Assert(err, jc.Satisfies, params.IsCodeNotImplemented)
}

func (s *clientSuite) TestWatchDebugLogConnected(c *gc.C) {
	// Shows both the unmarshalling of a real error, and
	// that the api server is connected.
	client := s.APIState.Client()
	reader, err := client.WatchDebugLog(api.DebugLogParams{})
	c.Assert(err, gc.ErrorMatches, "cannot open log file: .*")
	c.Assert(reader, gc.IsNil)
}

func (s *clientSuite) TestConnectionErrorBadConnection(c *gc.C) {
	s.PatchValue(api.DialDebugLog, func(_ *websocket.Config) (io.ReadCloser, error) {
		return nil, fmt.Errorf("bad connection")
	})
	client := s.APIState.Client()
	reader, err := client.WatchDebugLog(api.DebugLogParams{})
	c.Assert(err, gc.ErrorMatches, "bad connection")
	c.Assert(reader, gc.IsNil)
}

func (s *clientSuite) TestConnectionErrorNoData(c *gc.C) {
	s.PatchValue(api.DialDebugLog, func(_ *websocket.Config) (io.ReadCloser, error) {
		return &closableBuffer{&bytes.Buffer{}, c}, nil
	})
	client := s.APIState.Client()
	reader, err := client.WatchDebugLog(api.DebugLogParams{})
	c.Assert(err, gc.ErrorMatches, "unable to read initial response: EOF")
	c.Assert(reader, gc.IsNil)
}

func (s *clientSuite) TestConnectionErrorBadData(c *gc.C) {
	s.PatchValue(api.DialDebugLog, func(_ *websocket.Config) (io.ReadCloser, error) {
		junk := bytes.NewBufferString("junk\n")
		return &closableBuffer{junk, c}, nil
	})
	client := s.APIState.Client()
	reader, err := client.WatchDebugLog(api.DebugLogParams{})
	c.Assert(err, gc.ErrorMatches, "unable to unmarshal initial response: .*")
	c.Assert(reader, gc.IsNil)
}

func (s *clientSuite) TestConnectionErrorReadError(c *gc.C) {
	s.PatchValue(api.DialDebugLog, func(_ *websocket.Config) (io.ReadCloser, error) {
		junk := bytes.NewBufferString("junk")
		reader := &closableBuffer{junk, c}
		err := fmt.Errorf("bad read")
		return &badReader{reader, err}, nil
	})
	client := s.APIState.Client()
	reader, err := client.WatchDebugLog(api.DebugLogParams{})
	c.Assert(err, gc.ErrorMatches, "unable to read initial response: bad read")
	c.Assert(reader, gc.IsNil)
}

func (s *clientSuite) TestParamsEncoded(c *gc.C) {
	s.PatchValue(api.DialDebugLog, echoUrl(c))

	params := api.DebugLogParams{
		IncludeEntity: []string{"a", "b"},
		IncludeModule: []string{"c", "d"},
		ExcludeEntity: []string{"e", "f"},
		ExcludeModule: []string{"g", "h"},
		Limit:         100,
		Backlog:       200,
		Level:         loggo.ERROR,
		Replay:        true,
	}

	client := s.APIState.Client()
	reader, err := client.WatchDebugLog(params)
	c.Assert(err, gc.IsNil)

	bufReader := bufio.NewReader(reader)
	location, err := bufReader.ReadString('\n')
	c.Assert(err, gc.IsNil)
	url, err := url.Parse(strings.TrimSpace(location))
	c.Assert(err, gc.IsNil)

	values := url.Query()
	c.Assert(values["includeEntity"], jc.SameContents, params.IncludeEntity)
	c.Assert(values["includeModule"], jc.SameContents, params.IncludeModule)
	c.Assert(values["excludeEntity"], jc.SameContents, params.ExcludeEntity)
	c.Assert(values["excludeModule"], jc.SameContents, params.ExcludeModule)
	c.Assert(values.Get("maxLines"), gc.Equals, "100")
	c.Assert(values.Get("backlog"), gc.Equals, "200")
	c.Assert(values.Get("level"), gc.Equals, "ERROR")
	c.Assert(values.Get("replay"), gc.Equals, "true")
}

// bytes.Buffer is an io.Reader, but not an io.ReadCloser.
// closeableBuffer provides a no-op close method for the buffer.
type closableBuffer struct {
	*bytes.Buffer
	check *gc.C
}

func (r *closableBuffer) Read(p []byte) (n int, err error) {
	r.check.Logf("read, %v", len(p))
	n, err = r.Buffer.Read(p)
	r.check.Logf("  result: %q, %v, %v", string(p[:n]), n, err)
	return
}

func (*closableBuffer) Close() error {
	return nil
}

// badReader raises err when read is attempted
type badReader struct {
	io.ReadCloser
	err error
}

func (r *badReader) Read(p []byte) (n int, err error) {
	return 0, r.err
}

func echoUrl(c *gc.C) func(*websocket.Config) (io.ReadCloser, error) {
	response := &params.ErrorResult{}
	message, err := json.Marshal(response)
	c.Assert(err, gc.IsNil)
	return func(config *websocket.Config) (io.ReadCloser, error) {
		testBuff := fmt.Sprintf("%s\n%s\n", string(message), config.Location.String())
		c.Logf("test buffer: %v", testBuff)
		buff := bytes.NewBufferString(testBuff)
		return &closableBuffer{buff, c}, nil
	}
}
