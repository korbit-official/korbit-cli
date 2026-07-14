// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package keymgmtcmd

import (
	"fmt"
	"io"
	"strconv"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/cli/textout"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/progname"
)

// Result types and human rendering for the `key *` / `setup` commands. Each type
// is emitted by keycmd.go and implements textout.TextFormatter, so the emitter
// renders the human text from typed fields and marshals the same struct for
// --json. The whole outcome — including the registration link, the numbered next
// steps, and the public key — is rendered here onto STDOUT (it is the command's
// result, the same data the --json document carries); stderr is reserved for
// progress and warnings (see the output rule in keycmd.go). The lower-layer keys.*
// results (rename/remove/show) are wrapped in embedding views so internal/keys
// need not depend on the human-output toolkit.

// writeRegistrationGuidance renders the human stdout block for a freshly
// generated / still-unbound key: the headline, the numbered next steps (which
// lead with the portal deep link when one could be built), and the public key as
// a labeled fallback for manual registration. It is shared by newKeyResult and
// setupResumeResult so `key add`, `setup`, and the resume path read identically.
func writeRegistrationGuidance(w io.Writer, headline string, steps []string, publicPEM, link string) {
	fmt.Fprintf(w, "%s\n\n%s\n%s", headline, i18n.T("Next steps:"), numbered(steps))
	if publicPEM != "" {
		label := i18n.T("Prefer to register manually? Paste this public key (ED25519) at the portal instead:")
		if link == "" {
			label = i18n.T("Public key (paste this into the developers portal):")
		}
		fmt.Fprintf(w, "\n\n%s\n%s", label, publicPEM)
	}
}

// newKeyResult is `setup` / `key add` for a freshly generated, not-yet-bound key
// (it still needs portal registration).
type newKeyResult struct {
	Name             string        `json:"name"`
	Type             string        `json:"type"`
	Keystore         string        `json:"keystore"`
	PublicKey        string        `json:"publicKey"`
	IsDefault        bool          `json:"isDefault"`
	Bound            bool          `json:"bound"`
	Status           string        `json:"status"`
	RegistrationURL  string        `json:"registrationUrl"`
	RegistrationLink string        `json:"registrationLink,omitempty"`
	IPAllowlist      *probe.Report `json:"ipAllowlist,omitempty"`
	Next             []string      `json:"next"`
}

func (r newKeyResult) FormatText(w io.Writer) {
	headline := i18n.T("Generated ED25519 key %q — private key stored in the %s keystore.", r.Name, r.Keystore)
	writeRegistrationGuidance(w, headline, r.Next, r.PublicKey, r.RegistrationLink)
}

// boundKeyResult is `key add --api-key`: created and bound in one step (already
// registered), so there is no link to print.
type boundKeyResult struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Keystore  string   `json:"keystore"`
	PublicKey string   `json:"publicKey"`
	APIKeyID  string   `json:"apiKeyId"`
	IsDefault bool     `json:"isDefault"`
	Bound     bool     `json:"bound"`
	Status    string   `json:"status"`
	Next      []string `json:"next"`
}

func (r boundKeyResult) FormatText(w io.Writer) {
	fmt.Fprintf(w, "Added key %q in the %s keystore and bound it to apiKeyId %s.", r.Name, r.Keystore, r.APIKeyID)
	if len(r.Next) > 0 {
		fmt.Fprintf(w, "\n\nNext step:\n%s", numbered(r.Next))
	}
}

// keyBindResult is `key bind`.
type keyBindResult struct {
	Name     string `json:"name"`
	APIKeyID string `json:"apiKeyId"`
	Bound    bool   `json:"bound"`
}

func (r keyBindResult) FormatText(w io.Writer) {
	fmt.Fprintf(w, "Key %q is bound to apiKeyId %s. Verify with `%s whoami --key %s`.", r.Name, r.APIKeyID, progname.Name(), r.Name)
}

// keyUseResult is `key use`.
type keyUseResult struct {
	DefaultKey string `json:"defaultKey"`
}

func (r keyUseResult) FormatText(w io.Writer) {
	fmt.Fprintf(w, "Default key set to %q.", r.DefaultKey)
}

// keyListResult is `key list`.
type keyListResult struct {
	Home            string         `json:"home"`
	DefaultKeystore string         `json:"defaultKeystore"` // backend for NEW keys; each key carries its own
	DefaultKey      *string        `json:"defaultKey"`
	Keys            []keys.Summary `json:"keys"`
}

func (r keyListResult) FormatText(w io.Writer) {
	defKey := ""
	if r.DefaultKey != nil {
		defKey = *r.DefaultKey
	}
	head := textout.KVBlock([][2]string{
		{"home", r.Home},
		{"default key", textout.OrNone(defKey)},
		{"new-key keystore", r.DefaultKeystore},
	})
	if len(r.Keys) == 0 {
		fmt.Fprint(w, head+"\n\n(no keys — create one with `"+progname.Name()+" setup`)")
		return
	}
	headers := []string{"", "name", "type", "keystore", "apiKey", "acctSeq", "baseUrl"}
	align := []bool{false, false, false, false, false, true, false}
	var rows [][]string
	for _, k := range r.Keys {
		marker := ""
		if k.IsDefault {
			marker = "*"
		}
		api := "(unbound)"
		if k.Bound && k.APIKeyID != nil && *k.APIKeyID != "" {
			api = *k.APIKeyID
		}
		name := k.Name
		if k.IsSandbox {
			name += " [sandbox]"
		}
		acctSeq := accountseq.MainString()
		if k.DefaultAccountSeq != nil {
			acctSeq = strconv.Itoa(*k.DefaultAccountSeq)
		}
		rows = append(rows, []string{marker, name, k.Type, k.Keystore, api, acctSeq, k.BaseURL})
	}
	fmt.Fprint(w, head+"\n\n"+textout.Table(headers, rows, align)+"\n\n(* = default key)")
}

// showView wraps the lower-layer keys.SummaryWithPublic for `key show`.
type showView struct{ keys.SummaryWithPublic }

func (v showView) FormatText(w io.Writer) {
	api := "(unbound)"
	if v.APIKeyID != nil && *v.APIKeyID != "" {
		api = *v.APIKeyID
	}
	acctSeq := ""
	if v.DefaultAccountSeq != nil {
		acctSeq = strconv.Itoa(*v.DefaultAccountSeq)
	}
	out := textout.KVBlock([][2]string{
		{"name", v.Name},
		{"type", v.Type},
		{"keystore", v.Keystore},
		{"apiKey", api},
		{"default", textout.YesNo(v.IsDefault)},
		{"defaultAccountSeq", acctSeq},
		{"baseUrl", v.BaseURL},
		{"wsBaseUrl", v.WSBaseURL},
		{"createdAt", strconv.FormatInt(v.CreatedAt, 10)},
	})
	if v.PublicKey != "" {
		out += "\n\npublicKey:\n" + v.PublicKey
	}
	fmt.Fprint(w, out)
}

// removeView wraps the lower-layer keys.RemoveResult for `key remove`.
type removeView struct {
	keys.RemoveResult
	Next []string `json:"next,omitempty"`
}

func (v removeView) FormatText(w io.Writer) {
	text := fmt.Sprintf("Removed key %q.", v.Removed)
	if v.Warning != "" {
		text += " Warning: " + v.Warning
	} else if !v.SecretRemoved {
		text += " Warning: its private key material was left in the backend."
	}
	if len(v.Next) > 0 {
		text += "\n\nNext step:\n" + numbered(v.Next)
	}
	fmt.Fprint(w, text)
}

// renameView wraps the lower-layer keys.RenameResult for `key rename`.
type renameView struct{ keys.RenameResult }

func (v renameView) FormatText(w io.Writer) {
	text := fmt.Sprintf("Renamed key %q to %q (keypair and binding preserved).", v.OldName, v.NewName)
	if v.IsDefault {
		text += " It is still the default key."
	}
	if v.Warning != "" {
		text += " Warning: " + v.Warning
	}
	fmt.Fprint(w, text)
}

// setBaseURLResult is `key set-base-url` (both the set and --clear forms; the
// optional verification is present only when the endpoints were smoke-tested).
type setBaseURLResult struct {
	Name         string                      `json:"name"`
	BaseURL      string                      `json:"baseUrl"`
	WSBaseURL    string                      `json:"wsBaseUrl"`
	Verification *probe.EndpointVerification `json:"verification,omitempty"`
}

func (r setBaseURLResult) FormatText(w io.Writer) {
	if r.BaseURL == "" {
		fmt.Fprintf(w, "Key %q reverted to the default API endpoint.", r.Name)
		return
	}
	fmt.Fprintf(w, "Key %q now uses base URL %s (WebSocket %s).", r.Name, r.BaseURL, r.WSBaseURL)
	// The best-effort endpoint smoke test is part of the result — render it on
	// stdout, not stderr.
	if r.Verification != nil {
		fmt.Fprintf(w, "\n\n%s", formatEndpointVerification(r.Name, r.BaseURL, *r.Verification))
	}
}

// setDefaultAccountSeqResult is `key set-default-account-seq` (set and --clear).
type setDefaultAccountSeqResult struct {
	Name              string `json:"name"`
	DefaultAccountSeq *int   `json:"defaultAccountSeq,omitempty"`
}

func (r setDefaultAccountSeqResult) FormatText(w io.Writer) {
	if r.DefaultAccountSeq == nil {
		fmt.Fprintf(w, "Key %q reverted to the default sub-account (main account %s).", r.Name, accountseq.MainString())
		return
	}
	fmt.Fprintf(w, "Key %q now defaults to sub-account %d.", r.Name, *r.DefaultAccountSeq)
}
