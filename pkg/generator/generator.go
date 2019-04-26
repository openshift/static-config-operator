package generator

import (
	"context"
)

type Interface interface {
	GenerateSyncSetList(ctx context.Context) error
	WriteDB(filepath string) error
}
