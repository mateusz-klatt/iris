package hubclient

import "os"

func openRegularConfig(path string) (*os.File, error) {
	return os.Open(path)
}
