package remotecloud

import (
	"net/url"
	"path"
	"strings"

	"reasonix/internal/platform/account"
)

// The tunnel's path→scope table is the security boundary for narrow grants:
// desktop reaches everything as before, a narrower scope only its mapped
// paths, and every unmapped path stays desktop's.

var tunnelPaths = map[string]account.RemoteCapability{
	"/workspace/files":           account.RemoteFiles,
	"/workspace/file":            account.RemoteFiles,
	"/workspace/image":           account.RemoteFiles,
	"/workspace/locate":          account.RemoteFiles,
	"/workspace/preview/info":    account.RemoteFiles,
	"/workspace/preview/content": account.RemoteFiles,
	"/attachments":               account.RemoteFiles,
	"/attachments/upload":        account.RemoteFiles,
}

// tunnelScope names the scope a path answers to besides desktop. The runtime
// mount serves the same routes under /rt/{id}/, and a dirty path only ever
// earns a redirect from the mux, so matching the cleaned path cannot grant
// more than the handler would serve.
func tunnelScope(raw string) account.RemoteCapability {
	target, err := url.ParseRequestURI(raw)
	if err != nil {
		return account.RemoteDesktop
	}
	clean := path.Clean(target.Path)
	if rest, ok := runtimePath(clean); ok {
		clean = rest
	}
	if clean == "/terminals" || strings.HasPrefix(clean, "/terminals/") {
		return account.RemoteTerminal
	}
	if scope, mapped := tunnelPaths[clean]; mapped {
		return scope
	}
	return account.RemoteDesktop
}

func runtimePath(clean string) (string, bool) {
	rest, ok := strings.CutPrefix(clean, "/rt/")
	if !ok {
		return "", false
	}
	id, tail, found := strings.Cut(rest, "/")
	if id == "" {
		return "", false
	}
	if !found {
		return "/", true
	}
	return "/" + tail, true
}

// tunnelAllowed is the gate every tunnelled request and upload part passes.
func tunnelAllowed(scopes []account.RemoteCapability, raw string) bool {
	return hasScope(scopes, account.RemoteDesktop) || hasScope(scopes, tunnelScope(raw))
}
