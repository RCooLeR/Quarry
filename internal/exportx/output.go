package exportx

import (
	"os"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

var removeFile = os.Remove

type createdOutput = fileio.ExclusiveOutput

func openCreatedOutput(path string) (*createdOutput, error) {
	return fileio.OpenExclusiveOutput(path, 0o600)
}
