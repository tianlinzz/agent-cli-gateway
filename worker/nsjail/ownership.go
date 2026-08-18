package nsjail

import (
	"fmt"
	"os"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

// EnsureJailOwnership makes one directory usable by the jailed worker when the
// gateway runs as root inside the container (the compat deployment for hosts
// whose LSM denies mounts created by NON-root user namespaces — there the
// userns must be created by root, see the README deployment notes).
//
// The jail maps the configured uid/gid (inside_id/outside_id both
// UserNamespace.UID/GID), so the nsjail child resolves bind-mount sources and
// the jailed CLIs write workspace/agent-home/socket files as that uid. Every
// directory the jail must traverse or write therefore has to be owned by it:
// root-created 0700 directories would fail the bind mounts with EACCES and
// break the jailed worker's writes.
//
// It is a no-op unless the process actually runs as euid 0 and the user
// namespace is enabled, so the default non-root (65532) deployment — where the
// gateway uid equals the jail uid and already owns everything it creates — is
// untouched, as are dev/test runs on non-root hosts.
func EnsureJailOwnership(path string, iso config.IsolationConfig) error {
	if !iso.UserNamespace.Enabled {
		return nil
	}
	uid, gid := iso.UserNamespace.UID, iso.UserNamespace.GID
	if uid <= 0 || gid <= 0 {
		// uid/gid 0 would be a misconfiguration for a de-privileging map and
		// is never what a root-run compat deployment wants; chown-to-root is
		// both pointless and silently wrong, so fail closed instead.
		return fmt.Errorf("nsjail: jail ownership: user_namespace uid/gid must be a positive unprivileged id (got uid=%d gid=%d)", uid, gid)
	}
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("nsjail: jail ownership: chown %q to %d:%d: %w", path, uid, gid, err)
	}
	return nil
}
