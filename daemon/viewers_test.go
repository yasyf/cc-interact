package daemon

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestActivityViewers(t *testing.T) {
	a := NewActivity()
	d1 := a.AttachViewer("s1")
	d2 := a.AttachViewer("s1")
	d1()
	if !a.Viewing("s1") {
		t.Fatal("one open viewer must still count as viewing")
	}
	d2()
	d2()
	if a.Viewing("s1") {
		t.Fatal("viewing after every viewer closed")
	}
	a.AttachViewer("s1")
	if a.Viewing("s2") {
		t.Fatal("viewer leaked across subjects")
	}
}

func TestViewerConnectedFollowsBrowserStream(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", 42, "open")
	srv := httptest.NewServer(s.sse.Handler())
	defer srv.Close()

	named, cancelNamed := openStream(t, srv.URL+"/events?session="+sub.ID+"&consumer=channel&claude_pid=42")
	if s.ViewerConnected(sub.ID) {
		t.Fatal("a named consumer counted as a viewer")
	}
	cancelNamed()
	_ = named.Body.Close()

	browser, cancelBrowser := openStream(t, srv.URL+"/events?session=slug1")
	if !s.ViewerConnected(sub.ID) {
		t.Fatal("ViewerConnected false with a browser stream open")
	}
	cancelBrowser()
	_ = browser.Body.Close()
	waitFor(t, "ViewerConnected false after the browser stream closed", func() bool {
		return !s.ViewerConnected(sub.ID)
	})
}

func openStream(t *testing.T, url string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream %s: %v", url, err)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), ": connected") {
			return resp, cancel
		}
	}
	t.Fatalf("stream %s ended before liveness comment", url)
	return nil, nil
}
