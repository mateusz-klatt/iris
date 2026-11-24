package hook

import "io"

// GenerationReadLimit bounds a generation or end-marker read. A file filling the bound is invalid.
const GenerationReadLimit = 64

var generationFileOpen = OpenGenerationFile

func readGenerationBytes(path string) ([]byte, error) {
	f, err := generationFileOpen(path)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, GenerationReadLimit))
	_ = f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if len(raw) == GenerationReadLimit {
		return nil, ErrInvalidGeneration
	}
	return raw, nil
}
