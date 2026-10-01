// Package cliout writes the CLI's machine-readable output, which the Neovim
// companion and scripts read as a versioned interface.
package cliout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// SchemaVersion numbers the JSON documents the CLI prints. Increase it when a
// field is removed or changes meaning; adding fields does not change it.
const SchemaVersion = 1

// JSON prints v, which must encode as an object, as one document on stdout.
func JSON(v any, indent bool) error { return Write(os.Stdout, v, indent) }

// Write encodes v with schema_version as its first field.
func Write(w io.Writer, v any, indent bool) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) < 2 || data[0] != '{' {
		return errors.New("CLI output must be a JSON object")
	}
	head := fmt.Sprintf(`{"schema_version":%d`, SchemaVersion)
	if string(data) != "{}" {
		head += ","
	}
	data = append([]byte(head), data[1:]...)
	if indent {
		var out bytes.Buffer
		if err := json.Indent(&out, data, "", "  "); err != nil {
			return err
		}
		data = out.Bytes()
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
