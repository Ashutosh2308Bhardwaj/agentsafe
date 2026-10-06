package agentsafe_test

import (
	"path/filepath"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/storetest"
)

func TestFileStoreConformance(t *testing.T) {
	paths := map[agentsafe.LineStore]string{}
	storetest.Run(t, storetest.Backend{
		NewRun: func(t *testing.T) agentsafe.LineStore {
			p := filepath.Join(t.TempDir(), "run.jsonl")
			s := agentsafe.NewFileStore(p)
			paths[s] = p
			return s
		},
		Reopen: func(_ *testing.T, s agentsafe.LineStore) agentsafe.LineStore {
			r := agentsafe.NewFileStore(paths[s])
			paths[r] = paths[s]
			return r
		},
	})
}
