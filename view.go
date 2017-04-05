package pilosa

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// View layout modes.
const (
	ViewStandard = "standard"
	ViewInverse  = "inverse"
)

// View represents a container for frame data.
type View struct {
	mu    sync.Mutex
	path  string
	db    string
	frame string
	name  string

	// Fragments by slice.
	fragments map[uint64]*Fragment

	stats StatsClient

	BitmapAttrStore *AttrStore
	LogOutput       io.Writer
}

// NewView returns a new instance of View.
func NewView(path, db, frame, name string) *View {
	return &View{
		path:  path,
		db:    db,
		frame: frame,
		name:  name,

		fragments: make(map[uint64]*Fragment),

		stats:     NopStatsClient,
		LogOutput: ioutil.Discard,
	}
}

// Name returns the name the view was initialized with.
func (v *View) Name() string { return v.name }

// DB returns the database name the view was initialized with.
func (v *View) DB() string { return v.db }

// Frame returns the frame name the view was initialized with.
func (v *View) Frame() string { return v.frame }

// Path returns the path the view was initialized with.
func (v *View) Path() string { return v.path }

// Open opens and initializes the view.
func (v *View) Open() error {
	if err := func() error {
		// Ensure the view's path exists.
		if err := os.MkdirAll(v.path, 0777); err != nil {
			return err
		} else if err := os.MkdirAll(filepath.Join(v.path, "fragments"), 0777); err != nil {
			return err
		}

		if err := v.openFragments(); err != nil {
			return err
		}

		return nil
	}(); err != nil {
		v.Close()
		return err
	}

	return nil
}

// openFragments opens and initializes the fragments inside the view.
func (v *View) openFragments() error {
	file, err := os.Open(filepath.Join(v.path, "fragments"))
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer file.Close()

	fis, err := file.Readdir(0)
	if err != nil {
		return err
	}

	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}

		// Parse filename into integer.
		slice, err := strconv.ParseUint(filepath.Base(fi.Name()), 10, 64)
		if err != nil {
			continue
		}

		frag := v.newFragment(v.FragmentPath(slice), slice)
		if err := frag.Open(); err != nil {
			return fmt.Errorf("open fragment: slice=%s, err=%s", frag.Slice(), err)
		}
		frag.BitmapAttrStore = v.BitmapAttrStore
		v.fragments[frag.Slice()] = frag

		v.stats.Count("maxSlice", 1)
	}

	return nil
}

// Close closes the view and its fragments.
func (v *View) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Close all fragments.
	for _, frag := range v.fragments {
		_ = frag.Close()
	}
	v.fragments = make(map[uint64]*Fragment)

	return nil
}

// MaxSlice returns the max slice in the view.
func (v *View) MaxSlice() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()

	var max uint64
	for slice := range v.fragments {
		if slice > max {
			max = slice
		}
	}
	return max
}

// FragmentPath returns the path to a fragment in the view.
func (v *View) FragmentPath(slice uint64) string {
	return filepath.Join(v.path, "fragments", strconv.FormatUint(slice, 10))
}

// Fragment returns a fragment in the view by slice.
func (v *View) Fragment(slice uint64) *Fragment {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.fragment(slice)
}

func (v *View) fragment(slice uint64) *Fragment { return v.fragments[slice] }

// Fragments returns a list of all fragments in the view.
func (v *View) Fragments() []*Fragment {
	v.mu.Lock()
	defer v.mu.Unlock()

	other := make([]*Fragment, 0, len(v.fragments))
	for _, fragment := range v.fragments {
		other = append(other, fragment)
	}
	return other
}

// CreateFragmentIfNotExists returns a fragment in the view by slice.
func (v *View) CreateFragmentIfNotExists(slice uint64) (*Fragment, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.createFragmentIfNotExists(slice)
}

func (v *View) createFragmentIfNotExists(slice uint64) (*Fragment, error) {
	// Find fragment in cache first.
	if frag := v.fragments[slice]; frag != nil {
		return frag, nil
	}

	// Initialize and open fragment.
	frag := v.newFragment(v.FragmentPath(slice), slice)
	if err := frag.Open(); err != nil {
		return nil, err
	}
	frag.BitmapAttrStore = v.BitmapAttrStore

	// Save to lookup.
	v.fragments[slice] = frag

	v.stats.Count("maxSlice", 1)

	return frag, nil
}

func (v *View) newFragment(path string, slice uint64) *Fragment {
	frag := NewFragment(path, v.db, v.frame, v.name, slice)
	frag.LogOutput = v.LogOutput
	frag.stats = v.stats.WithTags(fmt.Sprintf("slice:%d", slice))
	return frag
}

// SetBit sets a bit within the view.
func (v *View) SetBit(bitmapID, profileID uint64) (changed bool, err error) {
	slice := profileID / SliceWidth
	frag, err := v.CreateFragmentIfNotExists(slice)
	if err != nil {
		return changed, err
	}
	return frag.SetBit(bitmapID, profileID)
}

// ClearBit clears a bit within the view.
func (v *View) ClearBit(bitmapID, profileID uint64) (changed bool, err error) {
	slice := profileID / SliceWidth
	frag, err := v.CreateFragmentIfNotExists(slice)
	if err != nil {
		return changed, err
	}
	return frag.ClearBit(bitmapID, profileID)
}

// IsViewInverted returns true if the view is used for storing an inverted representation.
func IsViewInverted(name string) bool {
	return strings.HasPrefix(name, ViewInverse)
}

type viewSlice []*View

func (p viewSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p viewSlice) Len() int           { return len(p) }
func (p viewSlice) Less(i, j int) bool { return p[i].Name() < p[j].Name() }

// ViewInfo represents schema information for a view.
type ViewInfo struct {
	Name string `json:"name"`
}

type viewInfoSlice []*ViewInfo

func (p viewInfoSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p viewInfoSlice) Len() int           { return len(p) }
func (p viewInfoSlice) Less(i, j int) bool { return p[i].Name < p[j].Name }