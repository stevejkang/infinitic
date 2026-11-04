package rotator

import (
	"bytes"
	"encoding/json"
	"os"

	"github.com/stevejkang/infinitic/internal/atomicfile"
)

// IdentityFile is claude's ~/.claude.json. Its oauthAccount entry is display-only (claude's
// /status and statusline), but keeping it in step with the primary login avoids confusion.
type IdentityFile struct {
	Path string
}

// OAuthAccountFor returns the stored oauthAccount if it belongs to accountUUID.
func (f IdentityFile) OAuthAccountFor(accountUUID string) json.RawMessage {
	doc, err := f.read()
	if err != nil {
		return nil
	}
	raw := doc["oauthAccount"]
	var oa struct {
		AccountUUID string `json:"accountUuid"`
	}
	if raw == nil || json.Unmarshal(raw, &oa) != nil || oa.AccountUUID != accountUUID {
		return nil
	}
	return raw
}

// SetOAuthAccount replaces the oauthAccount entry, leaving every other key untouched.
func (f IdentityFile) SetOAuthAccount(oauthAccount json.RawMessage) error {
	if oauthAccount == nil {
		return nil
	}
	doc, err := f.read()
	if err != nil {
		return err
	}
	doc["oauthAccount"] = oauthAccount
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	return atomicfile.Write(f.Path, buf.Bytes(), 0o600) // ~/.claude.json is often a dotfiles symlink
}

func (f IdentityFile) read() (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}
