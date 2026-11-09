package accountimport

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestSourceURIPathFormsOnEveryOperatingSystem(t *testing.T) {
	for _, test := range []struct {
		name, filename, encoded, path string
	}{
		{"posix", "/private/source.sqlite", "file:///private/source.sqlite", "/private/source.sqlite"},
		{"posix-backslash", `/private/source\name.sqlite`, "file:///private/source%5Cname.sqlite", `/private/source\name.sqlite`},
		{"posix-reserved", "/private/ą name%?#&+.sqlite", "file:///private/%C4%85%20name%25%3F%23&+.sqlite", "/private/ą name%?#&+.sqlite"},
		{"drive", `C:\private\source.sqlite`, "file:///C:/private/source.sqlite", "/C:/private/source.sqlite"},
		{"drive-forward", "C:/private/source.sqlite", "file:///C:/private/source.sqlite", "/C:/private/source.sqlite"},
		{"drive-lowercase", `z:\private\source.sqlite`, "file:///z:/private/source.sqlite", "/z:/private/source.sqlite"},
		{"drive-reserved", `C:\private\ą name%?#&+.sqlite`, "file:///C:/private/%C4%85%20name%25%3F%23&+.sqlite", "/C:/private/ą name%?#&+.sqlite"},
		{"unc", `\\server\share\source.sqlite`, "file:////server/share/source.sqlite", "//server/share/source.sqlite"},
		{"unc-forward", "//server/share/source.sqlite", "file:////server/share/source.sqlite", "//server/share/source.sqlite"},
		{"unc-reserved", `\\server\share\name%?#.sqlite`, "file:////server/share/name%25%3F%23.sqlite", "//server/share/name%?#.sqlite"},
		{"extended-drive", `\\?\C:\private\source.sqlite`, "file:///%5C%5C%3F%5CC:%5Cprivate%5Csource.sqlite", `/\\?\C:\private\source.sqlite`},
		{"extended-unc", `\\?\UNC\server\share\source.sqlite`, "file:///%5C%5C%3F%5CUNC%5Cserver%5Cshare%5Csource.sqlite", `/\\?\UNC\server\share\source.sqlite`},
		{"extended-verbatim", `\\?\C:\private\name .sqlite`, "file:///%5C%5C%3F%5CC:%5Cprivate%5Cname%20.sqlite", `/\\?\C:\private\name .sqlite`},
		{"posix-colon-component", "/:/source.sqlite", "file:///:/source.sqlite", "/:/source.sqlite"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dsn := sourceURI(test.filename)
			base, _, ok := strings.Cut(dsn, "?")
			if !ok || base != test.encoded {
				t.Fatal("source path URI has the wrong scheme, escaping or root")
			}
			uri, err := url.Parse(dsn)
			if err != nil || uri.Scheme != "file" || uri.Host != "" || uri.Opaque != "" || uri.Fragment != "" || uri.Path != test.path {
				t.Fatal("source filename became an authority, query, fragment or different path")
			}
			want := url.Values{"mode": {"ro"}, "immutable": {"1"}, "_pragma": {"query_only(1)", "trusted_schema(0)", "busy_timeout(5000)"}}
			if !reflect.DeepEqual(uri.Query(), want) {
				t.Fatal("source filename altered the bounded read-only immutable query")
			}
		})
	}
}
