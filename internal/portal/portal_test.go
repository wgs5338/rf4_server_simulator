package portal

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rf4server/internal/store"
)

func TestPortalIdentityPurchaseReplayAndLogout(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []int64{42, 43} {
		if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL.Exec(`UPDATE profile SET gold=500`); err != nil {
		t.Fatal(err)
	}
	s := New(db, func(remote string) int64 {
		if strings.HasPrefix(remote, "192.0.2.1:") {
			return 42
		}
		return 43
	})
	call := func(method, path, token, body, remote, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://game.test"+path, strings.NewReader(body))
		r.RemoteAddr = remote
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	const own = "192.0.2.1:1000"
	if w := call("GET", "/portal/api/profile", "", "", own, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("POST", "/portal/api/session", "", "{}", own, "https://other.test"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w := call("POST", "/portal/api/session", "", "{}", own, "")
	var result struct {
		Token string `json:"token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Token == "" {
		t.Fatal(w.Body.String())
	}
	token := result.Token
	if w := call("GET", "/portal/api/profile", token, "", "192.0.2.2:1000", ""); w.Code != 401 {
		t.Fatal("session accepted from another device")
	}
	if w := call("GET", "/portal/api/profile", token, "", "192.0.2.1:2000", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"userId":42`) {
		t.Fatal(w.Body.String())
	}
	if w := call("POST", "/portal/api/membership/purchase", token, `{"uid":43,"planID":"premium_1d","requestID":"purchase-0001"}`, own, ""); w.Code != 400 {
		t.Fatal("accepted caller-selected uid")
	}
	for range 2 {
		if w := call("POST", "/portal/api/membership/purchase", token, `{"planID":"premium_1d","requestID":"purchase-0001"}`, own, ""); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	p, err := db.GetProfile(context.Background(), 42)
	other, _ := db.GetProfile(context.Background(), 43)
	if err != nil || p.Gold != 400 || p.MembershipExp <= time.Now().Unix() || other.Gold != 500 {
		t.Fatalf("purchase identity/replay failed: %+v %+v %v", p, other, err)
	}
	if w := call("DELETE", "/portal/api/session", token, "", own, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/portal/api/profile", token, "", own, ""); w.Code != 401 {
		t.Fatal("logout did not revoke session")
	}
	for _, path := range []string{"/portal/", "/portal/local.js", "/portal/local.css"} {
		if w := call("GET", path, "", "", own, ""); w.Code != 200 {
			t.Fatalf("missing asset %s: %d", path, w.Code)
		}
	}
	if w := call("GET", "/portal/api/debug", "", "", own, ""); w.Code != 404 {
		t.Fatal("excluded debug endpoint exposed")
	}
}


func TestNativeSentMailEndpointRequiresDeviceSession(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "native-mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureProfile(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	s := New(db, func(string) int64 { return 42 })
	call := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://game.test/portal/api/native-sent-mail", strings.NewReader(body))
		r.RemoteAddr = "192.0.2.1:1000"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := call("", `{}`); w.Code != 401 {
		t.Fatalf("unauthenticated native sent-mail request returned %d", w.Code)
	}
	login := httptest.NewRequest("POST", "https://game.test/portal/api/session", strings.NewReader("{}"))
	login.RemoteAddr = "192.0.2.1:1000"
	lw := httptest.NewRecorder()
	s.ServeHTTP(lw, login)
	var sessionResult struct {
		Token string `json:"token"`
	}
	if lw.Code != 200 || json.Unmarshal(lw.Body.Bytes(), &sessionResult) != nil || sessionResult.Token == "" {
		t.Fatalf("session creation failed: %d %s", lw.Code, lw.Body.String())
	}
	body := `{"through":"","afterPeer":"","peer":"","before":"","read":0}`
	w := call(sessionResult.Token, body)
	var page nativeSentMailPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("native sent-mail page invalid: %d %s", w.Code, w.Body.String())
	}
	if page.Schema != 1 || page.Owner != "42" || page.Peers == nil || len(page.Peers) != 0 || page.Messages == nil || len(page.Messages) != 0 {
		t.Fatalf("native sent-mail page shape mismatch: %+v", page)
	}
	if w := call(sessionResult.Token, `{"through":"","after":"","count":50}`); w.Code != 400 {
		t.Fatalf("legacy guessed request fields should be rejected, got %d", w.Code)
	}
	if w := call(sessionResult.Token, `not-json`); w.Code != 400 {
		t.Fatalf("malformed native sent-mail request returned %d", w.Code)
	}
}

func TestNativeSentMailPagesSentMessagesAndAttachments(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "native-mail-pages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []int64{42, 43, 44} {
		if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
			t.Fatal(err)
		}
	}
	attachmentJSON := `[{"guid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","item_id":"AV_shamp_1lg","qty":2}]`
	for _, row := range []struct {
		to, from, kind int64
		text, items    string
		created        float64
		read           int64
	}{
		{to: 43, from: 42, kind: 1, text: "item mail", items: attachmentJSON, created: 1700000001, read: 0},
		{to: 43, from: 42, kind: 0, text: "second message", created: 1700000002, read: 1},
		{to: 44, from: 42, kind: 0, text: "other recipient", created: 1700000003, read: 0},
		{to: 42, from: 43, kind: 0, text: "incoming only", created: 1700000004, read: 0},
	} {
		_, err := db.SQL.Exec(`
			INSERT INTO mail(uid,sender,kind,text,items_json,created_ts,read)
			VALUES(?,?,?,?,?,?,?)`,
			row.to, row.from, row.kind, row.text, row.items, row.created, row.read)
		if err != nil {
			t.Fatal(err)
		}
	}

	s := New(db, func(string) int64 { return 42 })
	login := httptest.NewRequest("POST", "https://game.test/portal/api/session", strings.NewReader("{}"))
	login.RemoteAddr = "192.0.2.1:1000"
	lw := httptest.NewRecorder()
	s.ServeHTTP(lw, login)
	var sessionResult struct {
		Token string `json:"token"`
	}
	if lw.Code != 200 || json.Unmarshal(lw.Body.Bytes(), &sessionResult) != nil || sessionResult.Token == "" {
		t.Fatalf("session creation failed: %d %s", lw.Code, lw.Body.String())
	}
	call := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://game.test/portal/api/native-sent-mail", strings.NewReader(body))
		r.RemoteAddr = "192.0.2.1:1000"
		r.Header.Set("Authorization", "Bearer "+sessionResult.Token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	peerResponse := call(`{"through":"","afterPeer":"","peer":"","before":"","read":0}`)
	var peerPage nativeSentMailPage
	if peerResponse.Code != 200 || json.Unmarshal(peerResponse.Body.Bytes(), &peerPage) != nil {
		t.Fatalf("peer page invalid: %d %s", peerResponse.Code, peerResponse.Body.String())
	}
	if peerPage.Owner != "42" || peerPage.Through != "3" || len(peerPage.Peers) != 2 ||
		peerPage.Peers[0] != 43 || peerPage.Peers[1] != 44 || peerPage.MorePeers {
		t.Fatalf("recipient page mismatch: %+v", peerPage)
	}

	messageResponse := call(`{"through":"3","afterPeer":"","peer":"43","before":"","read":0}`)
	var messagePage nativeSentMailPage
	if messageResponse.Code != 200 || json.Unmarshal(messageResponse.Body.Bytes(), &messagePage) != nil {
		t.Fatalf("message page invalid: %d %s", messageResponse.Code, messageResponse.Body.String())
	}
	if messagePage.Peer != "43" || messagePage.HasMore || len(messagePage.Messages) != 2 {
		t.Fatalf("message page count/cursor mismatch: %+v", messagePage)
	}
	newest := messagePage.Messages[0]
	if newest.Sequence != 2 || newest.Recipient != 43 || newest.Item.Meta.Sender != 42 ||
		newest.Item.Meta.Text != "second message" || newest.Item.Meta.Type != "message" || newest.Item.Status != 1 {
		t.Fatalf("newest message mapping mismatch: %+v", newest)
	}
	oldest := messagePage.Messages[1]
	if oldest.Sequence != 1 || oldest.Item.Meta.ID != "00000000000000000000000000000001" ||
		oldest.Item.Meta.Type != "item" || oldest.Item.Meta.Created != 1700000001 ||
		len(oldest.Item.Meta.Items) != 1 || oldest.Item.Meta.Items[0].ID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		oldest.Item.Meta.Items[0].SysID != "AV_shamp_1lg" || oldest.Item.Meta.Items[0].Count != 2 {
		t.Fatalf("attachment message mapping mismatch: %+v", oldest)
	}

	olderResponse := call(`{"through":"3","afterPeer":"","peer":"43","before":"2","read":0}`)
	var olderPage nativeSentMailPage
	if olderResponse.Code != 200 || json.Unmarshal(olderResponse.Body.Bytes(), &olderPage) != nil ||
		len(olderPage.Messages) != 1 || olderPage.Messages[0].Sequence != 1 {
		t.Fatalf("before cursor did not page older messages: %d %s", olderResponse.Code, olderResponse.Body.String())
	}
}
