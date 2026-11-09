package accountimport

import (
	"net/url"
	"strings"
)

// sourceURI encodes a resolved absolute filename without a URI authority. Drive
// paths need /C:/... so C: cannot become the host. UNC paths keep their leading
// pair of separators in the path, producing an empty authority, not a server
// authority that SQLite rejects without SQLITE_ALLOW_URI_AUTHORITY.
func sourceURI(filename string) string {
	switch {
	case strings.HasPrefix(filename, `\\?\`):
		// Preserve the Windows extended-length namespace and its backslashes.
		// SQLite's Windows VFS removes this leading slash before that namespace;
		// converting it to ordinary UNC/drive syntax would lose verbatim paths.
		filename = "/" + filename
	case strings.HasPrefix(filename, `\\`):
		filename = strings.ReplaceAll(filename, `\`, "/")
	case len(filename) >= 3 && ((filename[0] >= 'A' && filename[0] <= 'Z') || (filename[0] >= 'a' && filename[0] <= 'z')) && filename[1] == ':' && (filename[2] == '\\' || filename[2] == '/'):
		filename = "/" + strings.ReplaceAll(filename, `\`, "/")
	}
	u := url.URL{Scheme: "file", Path: filename}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "trusted_schema(0)")
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()
	return u.String()
}
