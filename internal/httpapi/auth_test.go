package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/app"
)

// console is an operator signed in to the console: a session cookie plus its CSRF token.
type console struct {
	h            *harness
	cookie, csrf string
}

func signIn(h *harness, username, password string) console {
	h.t.Helper()
	resp := h.anonymous().post("/auth/login", map[string]any{"username": username, "password": password})
	session := resp.expect(200)
	var cookie *http.Cookie
	for _, c := range (&http.Response{Header: resp.header}).Cookies() {
		if c.Name == "zirnis_session" {
			cookie = c
		}
	}
	check(h.t, cookie != nil && cookie.HttpOnly && cookie.SameSite == http.SameSiteStrictMode, "session cookie must be HttpOnly and SameSite=Strict: ", resp.header)
	return console{h, cookie.Value, session.str("csrfToken")}
}

func (c console) send(method, path string, body any, csrf bool) *response {
	c.h.t.Helper()
	headers := map[string]string{"Cookie": "zirnis_session=" + c.cookie}
	if csrf {
		headers["X-CSRF-Token"] = c.csrf
		headers["Idempotency-Key"] = uuid.NewString()
	}
	return c.h.anonymous().send(method, path, body, headers)
}

func (c console) get(path string) *response            { return c.send("GET", path, nil, false) }
func (c console) post(path string, body any) *response { return c.send("POST", path, body, true) }

func TestConsoleOperatorsSignInWithAPasswordAndASession(t *testing.T) {
	h := newHarness(t)
	eq(t, 401, h.anonymous().get("/ops/parameters/pending").status) // nothing without signing in
	eq(t, "invalid_credentials", h.anonymous().post("/auth/login", map[string]any{"username": "amina", "password": "wrong-password-123"}).expect(401).str("code"))
	eq(t, "invalid_credentials", h.anonymous().post("/auth/login", map[string]any{"username": "nobody", "password": "wrong-password-123"}).expect(401).str("code"))

	amina := signIn(h, "amina", app.DemoOperatorPassword)
	me := amina.get("/auth/session").expect(200)
	eq(t, "amina", me.str("username"))
	eq(t, "maker", me.items("permissions")[0].v.(string))
	eq(t, 1, len(me.items("permissions")))

	// A change made with the cookie alone (a forged cross-site form) is refused.
	acct := h.fundedAccount(10_000, "Individual")
	body := map[string]any{"restriction": "DebitBlock", "reason": "Court order"}
	eq(t, "csrf_failed", amina.send("POST", "/ops/accounts/"+acct.ID+"/restriction", body, false).expect(403).str("code"))
	eq(t, 202, amina.post("/ops/accounts/"+acct.ID+"/restriction", body).status)

	// Signed out, the cookie is worthless.
	eq(t, 204, amina.post("/auth/logout", nil).status)
	eq(t, 401, amina.get("/auth/session").status)

	// So is a session left idle.
	brian := signIn(h, "brian", app.DemoOperatorPassword)
	eq(t, 200, brian.get("/ops/parameters/pending").status)
	h.clock.Advance(31 * time.Minute)
	eq(t, 401, brian.get("/ops/parameters/pending").status)
}

func TestRepeatedWrongPasswordsLockTheOperatorOut(t *testing.T) {
	h := newHarness(t)
	for range 5 {
		h.anonymous().post("/auth/login", map[string]any{"username": "amina", "password": "not-the-password"}).expect(401)
	}
	eq(t, "operator_locked", h.anonymous().post("/auth/login", map[string]any{"username": "amina", "password": app.DemoOperatorPassword}).expect(422).str("code"))
	h.clock.Advance(16 * time.Minute)
	signIn(h, "amina", app.DemoOperatorPassword)
}

func TestMakersInputCheckersAuthoriseAndNeitherDoesTheOthersJob(t *testing.T) {
	h := newHarness(t)
	amina, brian := signIn(h, "amina", app.DemoOperatorPassword), signIn(h, "brian", app.DemoOperatorPassword)
	acct := h.fundedAccount(10_000, "Individual")

	// The checker cannot input…
	eq(t, "permission_required", brian.post("/ops/risk/blocklist", map[string]any{"value": "+254711000222", "reason": "Mule"}).expect(403).str("code"))
	// …the maker's input changes nothing until it is authorised…
	pending := amina.post("/ops/accounts/"+acct.ID+"/restriction", map[string]any{"restriction": "DebitBlock", "reason": "Court order 114/2026"}).expect(202)
	eq(t, "None", h.platform().get("/accounts/"+acct.ID).expect(200).str("restriction"))
	// …and the maker cannot authorise, their own input or anyone's.
	eq(t, "permission_required", amina.post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(403).str("code"))

	authorised := brian.post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)
	eq(t, "amina", authorised.str("inputter"))
	eq(t, "brian", authorised.str("authoriser"))
	eq(t, "DebitBlock", h.platform().get("/accounts/"+acct.ID).expect(200).str("restriction"))
}

func TestOperatorsAreMaintainedUnderMakerCheckerAndChangeTheirOneTimePassword(t *testing.T) {
	h := newHarness(t)
	admin, brian, amina := signIn(h, "admin", app.DemoOperatorPassword), signIn(h, "brian", app.DemoOperatorPassword), signIn(h, "amina", app.DemoOperatorPassword)
	carol := map[string]any{"name": "Carol Wambui", "email": "carol@zirnis.example", "permissions": []string{"maker", "checker"}}

	eq(t, "permission_required", amina.post("/ops/operators/carol", carol).expect(403).str("code")) // only administrators
	eq(t, "self_maintenance", admin.post("/ops/operators/admin", map[string]any{"name": "Me", "email": "me@zirnis.example",
		"permissions": []string{"admin", "maker", "checker"}}).expect(422).str("code"))

	pending := admin.post("/ops/operators/carol", carol).expect(202)
	eq(t, 404, admin.get("/ops/operators/carol").status) // not until another administrator authorises
	brian.post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)

	// An administrator hands over a one-time password; carol must change it before anything else.
	eq(t, "weak_password", admin.post("/ops/operators/carol/password", map[string]any{"temporaryPassword": "short"}).expect(422).str("code"))
	admin.post("/ops/operators/carol/password", map[string]any{"temporaryPassword": "Temporary-Pass-0042"}).expect(200)
	c := signIn(h, "carol", "Temporary-Pass-0042")
	check(t, c.get("/auth/session").expect(200).boolean("mustChangePassword"), "a one-time password must be changed")
	eq(t, "password_change_required", c.get("/ops/parameters/pending").expect(403).str("code"))
	eq(t, "invalid_credentials", c.post("/auth/password", map[string]any{"currentPassword": "nope-nope-nope", "newPassword": "Wambui-Own-Secret-1"}).expect(422).str("code"))
	c.post("/auth/password", map[string]any{"currentPassword": "Temporary-Pass-0042", "newPassword": "Wambui-Own-Secret-1"}).expect(200)
	eq(t, 200, c.get("/ops/parameters/pending").status)

	// Deactivating an operator signs them out at once.
	pending = admin.post("/ops/operators/carol", map[string]any{"name": "Carol Wambui", "email": "carol@zirnis.example",
		"permissions": []string{"maker"}, "active": false}).expect(202)
	brian.post("/ops/parameters/pending/"+pending.str("id")+"/authorise", nil).expect(200)
	eq(t, 401, c.get("/auth/session").status)
	eq(t, "operator_inactive", h.anonymous().post("/auth/login", map[string]any{"username": "carol", "password": "Wambui-Own-Secret-1"}).expect(422).str("code"))

	// The record is versioned; the password never appears in it.
	history := admin.get("/ops/history/Operator/" + admin.get("/ops/operators/carol").expect(200).str("id")).expect(200).items()
	check(t, len(history) >= 1, "operator history")
	for _, version := range history {
		check(t, !strings.Contains(version.str("snapshot"), "argon2"), "password hash leaked into history")
	}
}

func TestDisputesAndSettlementsMoveMoneyOnlyWhenAuthorised(t *testing.T) {
	h := newHarness(t)
	merchant := h.account(h.verifiedCustomer("Business"))
	paymentID := h.cardPayment(merchant, 100_000, "tok_visa").str("id")

	pending := h.amina().postIdempotent("/ops/disputes", map[string]any{"paymentId": paymentID, "reason": "fraudulent"}).expect(202)
	eq(t, 0, h.locked(merchant.ID)) // nothing held yet
	h.brian().post("/ops/parameters/pending/"+pending.str("id")+"/reject", map[string]any{"reason": "Not a chargeback: customer enquiry"}).expect(200)
	eq(t, 0, h.locked(merchant.ID))
	eq(t, 0, len(h.platform().get("/disputes").expect(200).items()))

	openDispute(h, map[string]any{"paymentId": paymentID, "reason": "fraudulent"})
	eq(t, 100_000, h.locked(merchant.ID))
	h.assertLedgerBalanced()
}
