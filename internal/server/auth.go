package server

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/model"
)

const authRealm = "CUPS-Golang"

var nonceSecret = func() []byte {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err == nil {
		return buf
	}
	return []byte(strconv.FormatInt(time.Now().UnixNano(), 10))
}()

func (s *Server) authTypeForRequest(r *http.Request, op string) string {
	if s == nil {
		return ""
	}
	if r == nil {
		return ""
	}
	if rule := s.Policy.Match(r.URL.Path); rule != nil {
		if op != "" {
			if limit := s.Policy.LimitFor(r.URL.Path, op); limit != nil {
				if limit.AuthType != "" {
					return s.normalizeAuthType(limit.AuthType)
				}
			}
		}
		if rule.AuthType != "" {
			return s.normalizeAuthType(rule.AuthType)
		}
	}
	return ""
}

func (s *Server) normalizeAuthType(authType string) string {
	authType = strings.TrimSpace(authType)
	if authType == "" {
		return strings.TrimSpace(s.Config.DefaultAuthType)
	}
	// cupsd.conf supports `AuthType Default`, which maps to DefaultAuthType.
	if strings.EqualFold(authType, "default") {
		return strings.TrimSpace(s.Config.DefaultAuthType)
	}
	return authType
}

func (s *Server) requireAdmin(r *http.Request) bool {
	return s.authorize(r, s.authTypeForRequest(r, ""), true)
}

func (s *Server) requireAdminOr401(w http.ResponseWriter, r *http.Request) bool {
	return s.requireAuthOr401(w, r, true, "")
}

func (s *Server) requireAuthOr401(w http.ResponseWriter, r *http.Request, requireAdmin bool, op string) bool {
	authType := s.authTypeForRequest(r, op)
	u, ok := s.authenticate(r, authType)
	if ok {
		if !requireAdmin || u.IsAdmin {
			return true
		}
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	setAuthChallenge(w, authType)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
	return false
}

func (s *Server) authorize(r *http.Request, authType string, requireAdmin bool) bool {
	user, ok := s.authenticate(r, authType)
	if !ok {
		return false
	}
	if requireAdmin && !user.IsAdmin {
		return false
	}
	return true
}

func (s *Server) authenticate(r *http.Request, authType string) (model.User, bool) {
	if r == nil {
		return model.User{}, false
	}
	authType = strings.TrimSpace(authType)
	if strings.EqualFold(authType, "none") {
		return model.User{}, true
	}
	if authType == "" || strings.EqualFold(authType, "basic") {
		if u, ok := s.authenticateBasic(r); ok {
			return u, true
		}
		if authType != "" {
			return model.User{}, false
		}
	}
	if authType == "" || strings.EqualFold(authType, "digest") {
		if u, ok := s.authenticateDigest(r); ok {
			return u, true
		}
	}
	return model.User{}, false
}

func (s *Server) authenticateBasic(r *http.Request) (model.User, bool) {
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" {
		return model.User{}, false
	}
	var result model.User
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		u, err := s.Store.VerifyUser(r.Context(), tx, user, pass)
		if err != nil {
			return err
		}
		result = u
		return nil
	})
	if err != nil {
		return model.User{}, false
	}
	if result.DigestHA1 == "" && pass != "" {
		digest := computeDigestHA1(result.Username, pass)
		_ = s.Store.WithTx(r.Context(), false, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(), `UPDATE users SET digest_ha1 = ? WHERE username = ?`, digest, result.Username)
			return err
		})
		result.DigestHA1 = digest
	}
	return result, true
}

func (s *Server) authenticateDigest(r *http.Request) (model.User, bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(strings.ToLower(auth), "digest ") {
		return model.User{}, false
	}
	fields := parseDigestAuth(auth[len("Digest "):])
	username := fields["username"]
	realm := fields["realm"]
	nonce := fields["nonce"]
	uri := fields["uri"]
	response := fields["response"]
	qop := fields["qop"]
	nc := fields["nc"]
	cnonce := fields["cnonce"]
	if username == "" || nonce == "" || uri == "" || response == "" {
		return model.User{}, false
	}
	if realm != "" && realm != authRealm {
		return model.User{}, false
	}
	if !validateNonce(nonce) {
		return model.User{}, false
	}

	var user model.User
	err := s.Store.WithTx(r.Context(), true, func(tx *sql.Tx) error {
		u, err := s.Store.GetUserByUsername(r.Context(), tx, username)
		if err != nil {
			return err
		}
		user = u
		return nil
	})
	if err != nil || user.DigestHA1 == "" {
		return model.User{}, false
	}

	ha2 := md5Hex(fmt.Sprintf("%s:%s", r.Method, uri))
	var expected string
	if qop != "" {
		expected = md5Hex(fmt.Sprintf("%s:%s:%s:%s:%s:%s", user.DigestHA1, nonce, nc, cnonce, qop, ha2))
	} else {
		expected = md5Hex(fmt.Sprintf("%s:%s:%s", user.DigestHA1, nonce, ha2))
	}
	if strings.EqualFold(expected, response) {
		return user, true
	}
	return model.User{}, false
}

func setAuthChallenge(w http.ResponseWriter, authType string) {
	if strings.EqualFold(authType, "basic") {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+authRealm+`"`)
		return
	}
	if strings.EqualFold(authType, "digest") {
		nonce := generateNonce()
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", qop="auth", nonce="%s", algorithm=MD5`, authRealm, nonce))
		return
	}
	nonce := generateNonce()
	w.Header().Add("WWW-Authenticate", `Basic realm="`+authRealm+`"`)
	w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", qop="auth", nonce="%s", algorithm=MD5`, authRealm, nonce))
}

func parseDigestAuth(value string) map[string]string {
	out := map[string]string{}
	parts := []string{}
	var buf strings.Builder
	inQuotes := false
	for _, r := range value {
		switch r {
		case '"':
			inQuotes = !inQuotes
			buf.WriteRune(r)
		case ',':
			if inQuotes {
				buf.WriteRune(r)
			} else {
				parts = append(parts, buf.String())
				buf.Reset()
			}
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		parts = append(parts, buf.String())
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, "\"")
		out[strings.ToLower(key)] = val
	}
	return out
}

func generateNonce() string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha256.Sum256([]byte(ts + ":" + hex.EncodeToString(nonceSecret)))
	raw := ts + ":" + hex.EncodeToString(sum[:])
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func validateNonce(nonce string) bool {
	raw, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return false
	}
	ts, sig := parts[0], parts[1]
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if time.Since(time.Unix(t, 0)) > 10*time.Minute {
		return false
	}
	sum := sha256.Sum256([]byte(ts + ":" + hex.EncodeToString(nonceSecret)))
	expected := hex.EncodeToString(sum[:])
	return expected == sig
}

func md5Hex(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func computeDigestHA1(username, password string) string {
	return md5Hex(username + ":" + authRealm + ":" + password)
}

func isMutatingIPP(op int) bool {
	switch op {
	case int(goipp.OpCupsAddModifyPrinter), int(goipp.OpCupsDeletePrinter), int(goipp.OpCupsAddModifyClass),
		int(goipp.OpCupsDeleteClass), int(goipp.OpCupsSetDefault), int(goipp.OpCupsMoveJob), int(goipp.OpCupsAcceptJobs), int(goipp.OpCupsRejectJobs),
		int(goipp.OpPausePrinter), int(goipp.OpResumePrinter),
		int(goipp.OpEnablePrinter), int(goipp.OpDisablePrinter),
		int(goipp.OpPausePrinterAfterCurrentJob), int(goipp.OpHoldNewJobs), int(goipp.OpReleaseHeldNewJobs),
		int(goipp.OpRestartPrinter), int(goipp.OpPauseAllPrinters), int(goipp.OpPauseAllPrintersAfterCurrentJob),
		int(goipp.OpResumeAllPrinters), int(goipp.OpRestartSystem), int(goipp.OpCancelJobs), int(goipp.OpPurgeJobs),
		int(goipp.OpHoldJob), int(goipp.OpReleaseJob), int(goipp.OpRestartJob), int(goipp.OpResumeJob),
		int(goipp.OpSetPrinterAttributes), int(goipp.OpSetJobAttributes), int(goipp.OpRenewSubscription):
		return true
	default:
		return false
	}
}
