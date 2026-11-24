package hook

import "io"

// GenerationReadLimit bounds a generation or end-marker read. A file filling the bound is invalid.
const GenerationReadLimit = 64

var generationFileOpen = OpenGenerationFile

func readGenerationBytes(path string) ([]byte, error) {
	return readGenerationBytesLimit(path, GenerationReadLimit)
}

func readGenerationBytesLimit(path string, limit int64) ([]byte, error) {
	f, err := generationFileOpen(path)
	if err != nil {
		return nil, err
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, statErr
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, ErrInvalidGeneration
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, limit))
	_ = f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if int64(len(raw)) == limit {
		return nil, ErrInvalidGeneration
	}
	return raw, nil
}
