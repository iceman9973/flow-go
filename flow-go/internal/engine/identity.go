package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/kodelyx/flow-go/flow-go/internal/auth"
	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

// Account identity.
//
// The account id used to be shortID(jar.Hash()) — a hash over *every* cookie in
// the jar. That made the identity a function of state that rotates constantly:
// the same signed-in account produced six different rows in under two hours on
// this machine, because a single cookie changing is enough to change the hash.
// The database then accumulates one row per rotation, and every counter, balance
// and history entry is split across them.
//
// The fix is to stop deriving identity from mutable state. Three things change:
//
//  1. The anchor is computed from the long-lived credential cookies only, not
//     from the whole jar. The cookies that rotate most (OSID, the per-service
//     session copies, and the __Secure-*PSIDTS timestamp cookies) are excluded.
//  2. When the anchor *is* new, continuity is proven before a new identity is
//     minted — see resolveIdentity. A rotation re-anchors the existing row
//     instead of orphaning it.
//  3. A stable upstream identifier wins over any cookie when one is available.
//     The Labs session carries a GAIA user id and an email, both of which are
//     stable for the life of the account.
//
// What is deliberately NOT done: asking Google for the account identity over a
// new endpoint. That would settle point 3 outright, but it is an extra call
// against an account with a documented history of UNUSUAL_ACTIVITY flags, and
// the cookie-derived anchor is sufficient without it.

// identityCookieNames are the cookies that identify the account rather than the
// session.
//
// These are Google's long-lived account credentials. They are issued together,
// survive for months, and are what the session is rebuilt from. SAPISID in
// particular is the credential this engine signs every batchexecute request
// with — so it cannot have rotated while the engine kept working, which is the
// property that makes it usable as an anchor.
//
// Excluded on purpose: LSID and the __Host-*PLSID copies (rotated per session),
// ACCOUNT_CHOOSER (rewritten whenever the signed-in set changes), OSID and
// __Secure-OSID (per-service sessions for Flow itself), and __Secure-1PSIDTS /
// __Secure-3PSIDTS (timestamp cookies, which exist to rotate).
var identityCookieNames = []string{
	"SID",
	"HSID",
	"SSID",
	"APISID",
	"SAPISID",
	"__Secure-1PSID",
	"__Secure-3PSID",
}

// Identity sources, recorded alongside the key so a later reader can tell a
// proof from an assumption.
const (
	identitySourceOverride      = "override"       // supplied by the caller
	identitySourceSession       = "session"        // GAIA id or email from the Labs session
	identitySourceCookieCore    = "cookie-core"    // long-lived credential cookies
	identitySourceReAnchored    = "re-anchored"    // same session, rotated cookies
	identitySourceAdoptedLegacy = "adopted-legacy" // pre-fingerprint row, adopted once
)

// Identity is one account's durable label and the evidence behind it.
type Identity struct {
	AccountID string // the label used everywhere else, e.g. acct-a22a66840223
	Key       string // the anchor this was matched on, stored as identity_key
	Source    string // one of the identitySource* constants
}

// identityMaterial returns the sorted name=value pairs of the identity cookies,
// which is the input the anchor hashes.
//
// Sorted here rather than relying on the jar's own ordering, so the anchor does
// not silently change if the jar's sort changes.
func identityMaterial(cookies []cookiejar.Cookie) string {
	type pair struct{ key, value string }
	var pairs []pair

	for _, ck := range cookies {
		for _, want := range identityCookieNames {
			if strings.EqualFold(ck.Name, want) && ck.Value != "" {
				pairs = append(pairs, pair{
					key:   strings.ToLower(ck.Domain) + "|" + ck.Name,
					value: ck.Value,
				})
			}
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })

	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.key+"="+p.value)
	}
	return strings.Join(parts, "; ")
}

// sapisidFingerprint hashes SAPISID, the credential that signs every request.
//
// It is the continuity proof: a different account carries a different SAPISID,
// so an unchanged fingerprint means an unchanged session even when the rest of
// the jar has rotated. Only the hash is kept — never the value.
func sapisidFingerprint(cookies []cookiejar.Cookie) string {
	for _, ck := range cookies {
		if strings.EqualFold(ck.Name, "SAPISID") && ck.Value != "" {
			sum := sha256.Sum256([]byte(ck.Value))
			return hex.EncodeToString(sum[:])[:16]
		}
	}
	return ""
}

// identityAnchor derives the anchor and the label for a new identity.
//
// The returned key is what gets stored in identity_key and matched on; the
// returned id is the human-facing label. Both are stable for the account, and
// both get an index suffix applied by identityKeyFor when more than one account
// is signed in — one browser shares one cookie jar, so the jar alone cannot tell
// two accounts apart.
func identityAnchor(jar *cookiejar.Jar, session *auth.Session) (key, id, source string) {
	if session != nil {
		if gaia := strings.TrimSpace(session.UserID); gaia != "" {
			return "gaia:" + gaia, "acct-g" + gaia, identitySourceSession
		}
		if email := strings.TrimSpace(strings.ToLower(session.Email)); email != "" {
			sum := sha256.Sum256([]byte(email))
			short := hex.EncodeToString(sum[:])[:12]
			return "email:" + short, "acct-h" + short, identitySourceSession
		}
	}

	if jar == nil {
		return "", "", identitySourceCookieCore
	}

	core := sha256.Sum256([]byte(identityMaterial(jar.Cookies())))
	short := hex.EncodeToString(core[:])[:12]
	return "core:" + short, "acct-" + short, identitySourceCookieCore
}

// withIndex appends the authuser suffix.
//
// Index 0 gets no suffix, so a single-account browser keeps the label shape it
// has always had.
func withIndex(value string, index int) string {
	if index <= 0 {
		return value
	}
	return fmt.Sprintf("%s-u%d", value, index)
}

// resolveIdentity decides which durable account a jar belongs to.
//
// Order of resolution:
//
//  1. An explicitly supplied id wins outright — it is a decision, not an
//     inference, and it is how a caller pins a label deliberately.
//  2. An anchor already on record is the same account; reuse the row.
//  3. The anchor is new. Before minting, look for proof that this is an
//     existing account whose cookies rotated:
//     - the most recent active row at this index carries the same SAPISID
//     fingerprint → same session, re-anchor onto it;
//     - that row predates fingerprinting, so there is nothing to compare →
//     adopt it once, and say so in the source.
//     Anything else is genuinely new.
//
// The rule never merges on a guess. A row with a *different* fingerprint is not
// a candidate, so a real account switch mints a new identity rather than
// silently absorbing someone else's history.
func (e *Engine) resolveIdentity(jar *cookiejar.Jar, session *auth.Session, index int) Identity {
	if override := strings.TrimSpace(e.opts.AccountID); override != "" {
		return Identity{AccountID: override, Key: override, Source: identitySourceOverride}
	}

	baseKey, baseID, source := identityAnchor(jar, session)
	if baseKey == "" {
		// No jar and no session: nothing to anchor on. Fall back to the label
		// the engine has always used in this case rather than inventing one.
		return Identity{AccountID: "account", Key: "", Source: source}
	}

	key := withIndex(baseKey, index)

	if existing, found := e.lookupAnchor(key); found {
		return Identity{AccountID: existing, Key: key, Source: source}
	}

	fingerprint := sapisidFingerprint(jar.Cookies())

	previous, found, err := e.store.MostRecentActiveAccount(index)
	if err != nil {
		// Not fatal: with no candidate there is nothing to re-anchor onto, and
		// minting a fresh label is the behaviour that existed before this file.
		log.Printf("engine: could not look up the previous identity at index %d: %v", index, err)
	}

	if found {
		switch {
		case previous.SapisidFingerprint != "" && previous.SapisidFingerprint == fingerprint:
			e.reAnchor(previous, key, jar, fingerprint, identitySourceReAnchored)
			return Identity{AccountID: previous.AccountID, Key: key, Source: identitySourceReAnchored}

		case previous.SapisidFingerprint == "":
			// A row written before fingerprints existed. There is no evidence
			// either way, so this is an adoption, not a proof — recorded as
			// such so nobody later reads it as one.
			e.reAnchor(previous, key, jar, fingerprint, identitySourceAdoptedLegacy)
			return Identity{AccountID: previous.AccountID, Key: key, Source: identitySourceAdoptedLegacy}
		}
	}

	return Identity{AccountID: withIndex(baseID, index), Key: key, Source: source}
}

// recordIdentity writes the account row and its anchor history.
//
// Split out of Bootstrap so that the write the resolution depends on is the same
// one the tests exercise. A test that reproduced these writes itself could pass
// while Bootstrap quietly wrote something else.
func (e *Engine) recordIdentity(identity Identity, jar *cookiejar.Jar, sku string, index int) {
	cookieHash := ""
	fingerprint := ""
	if jar != nil {
		cookieHash = jar.Hash()
		fingerprint = sapisidFingerprint(jar.Cookies())
	}

	authuser := index
	if err := e.store.UpsertAccount(store.Account{
		AccountID:          identity.AccountID,
		CookieHash:         cookieHash,
		SKU:                sku,
		Status:             "active",
		IdentityKey:        identity.Key,
		IdentitySource:     identity.Source,
		LastAuthuser:       &authuser,
		SapisidFingerprint: fingerprint,
	}); err != nil {
		log.Printf("engine: could not record account: %v", err)
	}

	// Keep the anchor history even on the ordinary path, so the table records
	// every anchor and jar this account has presented rather than only the
	// rotations.
	if err := e.store.RecordAnchor(identity.AccountID, identity.Key, cookieHash, identity.Source); err != nil {
		log.Printf("engine: could not record the identity anchor for %s: %v", identity.AccountID, err)
	}
}

// lookupAnchor returns the account id already recorded for an anchor.
//
// A lookup failure is not fatal: the caller then treats the anchor as new and
// mints a label, which is the same thing that happened before this file existed.
func (e *Engine) lookupAnchor(key string) (string, bool) {
	id, found, err := e.store.AccountByAnchor(key)
	if err != nil {
		log.Printf("engine: could not look up the identity anchor %s: %v", key, err)
		return "", false
	}
	return id, found
}

// reAnchor moves an existing account row onto a new anchor, keeping its id and
// therefore its history.
func (e *Engine) reAnchor(previous store.AccountRef, key string, jar *cookiejar.Jar, fingerprint, source string) {
	cookieHash := ""
	if jar != nil {
		cookieHash = jar.Hash()
	}

	if err := e.store.ReAnchorAccount(previous.AccountID, key, cookieHash, fingerprint, source); err != nil {
		log.Printf("engine: could not re-anchor %s onto %s: %v", previous.AccountID, key, err)
		return
	}

	log.Printf("engine: identity %s re-anchored from %s to %s (%s) — the cookies "+
		"rotated, the account did not change", previous.AccountID, previous.IdentityKey, key, source)
}
