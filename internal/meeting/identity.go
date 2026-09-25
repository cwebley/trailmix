// Package meeting maintains recording identity independently of transcription.
package meeting

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func NewID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}

func ValidID(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) == 32 && err == nil
}

// Read also understands the original transcription-owned identity. It never
// changes old evidence or needs a provider connection.
func Read(dir string) (string, error) {
	var id string
	for _, name := range []string{"meeting.json", filepath.Join("transcription", "meeting.json")} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		var m struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &m) != nil || m.ID == "" {
			return "", errors.New("invalid meeting identity")
		}
		if id != "" && id != m.ID {
			return "", errors.New("conflicting meeting identities")
		}
		id = m.ID
	}
	return id, nil
}

// Ensure is called under the recorder or transcription writer lock.
func Ensure(dir, proposed string) (string, error) {
	id, err := Read(dir)
	if err != nil {
		return "", err
	}
	if id != "" && proposed != "" && id != proposed {
		return "", errors.New("meeting identity mismatch")
	}
	if id == "" {
		id = proposed
	}
	if id == "" {
		id = NewID()
	}
	path := filepath.Join(dir, "meeting.json")
	if _, err := os.Stat(path); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := WriteJSON(path, struct {
		ID string `json:"id"`
	}{id}); err != nil {
		return "", err
	}
	return id, nil
}

func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(data, '\n'))
}

// WriteFile durably replaces an app-owned file with an owner-only atomic write.
func WriteFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".trailmix-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return fmt.Errorf("persist %s: %w", path, err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
