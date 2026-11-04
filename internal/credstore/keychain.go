package credstore

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

const securityBin = "/usr/bin/security"

// Keychain keeps credentials as generic-password items in the login keychain (macOS).
type Keychain struct {
	PrimaryService string // "Claude Code-credentials" unless overridden for tests
	PoolPrefix     string // must not look like "Claude Code-credentials-<hex>", or tools that scan for Claude Code logins would list it
	Account        string // keychain account name for items infinitic creates
}

func (k Keychain) Primary() Slot {
	return Slot{key: k.PrimaryService, Label: "keychain:" + k.PrimaryService}
}

func (k Keychain) Pool(accountID string) Slot {
	svc := k.PoolPrefix + "-" + accountID
	return Slot{key: svc, Label: "keychain:" + svc}
}

func (k Keychain) Isolated(configDir string) Slot {
	svc := claudeServiceFor(k.PrimaryService, configDir)
	return Slot{key: svc, Label: "keychain:" + svc}
}

const errItemNotFound = 44 // security(1) exit status

func (Keychain) Read(s Slot) (string, bool, error) {
	out, stderr, code := runSecurity(nil, "find-generic-password", "-s", s.key, "-w")
	switch code {
	case 0:
		return strings.TrimSuffix(out, "\n"), true, nil
	case errItemNotFound:
		return "", false, nil
	default:
		// Report a locked keychain or a denied ACL as an error. Treating it as an empty slot
		// would let a switch overwrite a login that is merely unreadable.
		return "", false, fmt.Errorf("keychain read of %q failed (exit %d): %s", s.key, code, strings.TrimSpace(stderr))
	}
}

func (k Keychain) Write(s Slot, secret string) error {
	// Update the existing item under its own account name: `-U` with a different account
	// would silently create a second item with the same service.
	account := k.existingAccount(s)
	if account == "" {
		account = k.Account
	}
	if err := addGenericPassword(s.key, account, secret); err != nil {
		return err
	}
	back, ok, err := k.Read(s)
	if err != nil {
		return err
	}
	if !ok || back != secret {
		return fmt.Errorf("keychain write of %q did not read back identically", s.key)
	}
	return nil
}

func (Keychain) Delete(s Slot) error {
	_, stderr, code := runSecurity(nil, "delete-generic-password", "-s", s.key)
	if code != 0 && code != errItemNotFound {
		return fmt.Errorf("keychain delete of %q failed (exit %d): %s", s.key, code, strings.TrimSpace(stderr))
	}
	return nil
}

var accountAttr = regexp.MustCompile(`"acct"<blob>="([^"]*)"`)

func (Keychain) existingAccount(s Slot) string {
	out, _, code := runSecurity(nil, "find-generic-password", "-s", s.key)
	if code != 0 {
		return ""
	}
	if m := accountAttr.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// interactiveLineLimit is security(1)'s line buffer in interactive mode.
const interactiveLineLimit = 4000

// errTooLarge: the only other way to hand security(1) a secret is argv, which every local user
// can read with ps(1). Refusing is safer than leaking the token pair.
var errTooLarge = errors.New("credential is too large to store safely with security(1)")

func addGenericPassword(service, account, secret string) error {
	// `security -i` reads the command from stdin, so the secret never appears in argv.
	line := fmt.Sprintf("add-generic-password -U -a %s -s %s -w %s\n", quote(account), quote(service), quoteSecret(secret))
	if len(line) >= interactiveLineLimit {
		return fmt.Errorf("keychain write of %q: %w (%d bytes)", service, errTooLarge, len(secret))
	}
	_, stderr, code := runSecurity([]byte(line), "-i")
	if code != 0 || strings.Contains(strings.ToLower(stderr), "error") {
		// stderr can echo the command line, secret included
		return fmt.Errorf("keychain write of %q failed (exit %d): %s", service, code, redact(stderr, secret))
	}
	return nil
}

func redact(s, secret string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, secret, "<redacted>"))
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func quoteSecret(s string) string {
	if strings.Contains(s, "'") {
		return quote(s)
	}
	return "'" + s + "'"
}

func runSecurity(stdin []byte, args ...string) (stdout, stderr string, exitCode int) {
	cmd := exec.Command(securityBin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out.String(), errb.String(), 0
	case errors.As(err, &exitErr):
		return out.String(), errb.String(), exitErr.ExitCode()
	default:
		return "", err.Error(), -1
	}
}
