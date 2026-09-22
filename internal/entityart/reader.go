// Package entityart reads thumbnails from installed DST assets without running Lua.
package entityart

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dont/internal/maprenderer"
)

var ErrInvalid = errors.New("invalid entity artwork request")
var ErrLimit = errors.New("entity artwork exceeds resource limits")
var identifier = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,79}$`)
var inventoryAtlas = regexp.MustCompile(`^images/inventoryimages[0-9]*\.xml$`)

const ttl = 5 * time.Minute
const maxFile = 8 << 20
const maxPNG = 128 << 10

func Valid(prefab, mod string) bool {
	return identifier.MatchString(prefab) && (mod == "" || identifier.MatchString(mod))
}

type source struct {
	texture string
	bundled bool
	element maprenderer.AtlasElement
}
type collection struct {
	err      error
	failures map[string]error
	root     string
	until    time.Time
	icons    map[string]source
	pngs     map[string][]byte
}
type Reader struct {
	gate        chan struct{}
	collections map[string]*collection
	textureKey  string
	texture     *image.NRGBA
	timer       *time.Timer
}

func NewReader() *Reader {
	return &Reader{gate: make(chan struct{}, 1), collections: make(map[string]*collection)}
}

// Default is shared by all installations in a process: at most one decode at a
// time, four indexes, 8 MiB of thumbnails, and one 16 MiB decoded texture.
var Default = NewReader()

// Read uses installation-owned roots. The caller cannot supply a filesystem path.
// Missing artwork is a successful empty result; unavailable nodes remain errors.
func (r *Reader) Read(ctx context.Context, server, workshop, prefab, mod string) ([]byte, error) {
	if !Valid(prefab, mod) {
		return nil, ErrInvalid
	}
	select {
	case r.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-r.gate }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.timer == nil {
		r.timer = time.AfterFunc(ttl, r.clearIdle)
	} else {
		r.timer.Reset(ttl)
	}
	root := filepath.Join(server, "data")
	if mod != "" {
		root = filepath.Join(server, "mods", mod)
		if strings.HasPrefix(mod, "workshop-") && workshop != "" {
			root = filepath.Join(workshop, strings.TrimPrefix(mod, "workshop-"))
		}
	}
	key := root + "\x00" + mod
	c := r.collections[key]
	if c == nil || time.Now().After(c.until) {
		if len(r.collections) >= 4 {
			r.collections = make(map[string]*collection)
		}
		r.texture = nil
		r.textureKey = ""
		c = &collection{failures: make(map[string]error), root: root, until: time.Now().Add(ttl), icons: make(map[string]source), pngs: make(map[string][]byte)}
		if err := c.index(ctx, mod != ""); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.err = err
		}
		r.collections[key] = c
	}
	if c.err != nil {
		return nil, c.err
	}
	if data, ok := c.pngs[prefab]; ok {
		return bytes.Clone(data), nil
	}
	src, ok := c.icons[prefab+".tex"]
	if !ok {
		src, ok = c.icons[prefab+".png"]
	}
	if !ok {
		return nil, nil
	}
	if err := c.failures[src.texture]; err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	textureKey := key + "\x00" + src.texture
	if r.textureKey != textureKey {
		data, err := c.read(src.texture, src.bundled, maxFile)
		if err != nil {
			c.failures[src.texture] = err
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		// Reject oversized textures BEFORE the decoder allocates their pixel buffer.
		if len(data) < 18 || binary.LittleEndian.Uint16(data[8:10]) > 2048 || binary.LittleEndian.Uint16(data[10:12]) > 2048 {
			c.failures[src.texture] = ErrLimit
			return nil, ErrLimit
		}
		r.texture = nil
		r.textureKey = ""
		decoded, err := maprenderer.DecodeKTEX(data)
		if err != nil {
			c.failures[src.texture] = err
			return nil, err
		}
		r.texture = decoded
		r.textureKey = textureKey
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounds, err := src.element.Bounds(r.texture)
	if err != nil {
		return nil, err
	}
	// KTEX rows and atlas V coordinates start at the bottom. Flip just the crop.
	width, height := bounds.Dx(), bounds.Dy()
	scale := max(1, float64(max(width, height))/128)
	icon := image.NewNRGBA(image.Rect(0, 0, max(1, int(float64(width)/scale)), max(1, int(float64(height)/scale))))
	for y := 0; y < icon.Bounds().Dy(); y++ {
		for x := 0; x < icon.Bounds().Dx(); x++ {
			icon.SetNRGBA(x, y, r.texture.NRGBAAt(bounds.Min.X+min(width-1, int(float64(x)*scale)), bounds.Max.Y-1-min(height-1, int(float64(y)*scale))))
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, icon); err != nil {
		return nil, err
	}
	if output.Len() > maxPNG {
		return nil, ErrLimit
	}
	cachedBytes, cachedCount := 0, 0
	for _, entry := range r.collections {
		for _, data := range entry.pngs {
			cachedBytes += len(data)
			cachedCount++
		}
	}
	if cachedBytes+output.Len() > 8<<20 || cachedCount >= 1024 {
		for _, entry := range r.collections {
			entry.pngs = make(map[string][]byte)
		}
	}
	c.pngs[prefab] = output.Bytes()
	return bytes.Clone(output.Bytes()), nil
}
func (r *Reader) clearIdle() {
	r.gate <- struct{}{}
	defer func() { <-r.gate }()
	// A concurrent read may have reset the timer while this callback was waiting.
	for _, c := range r.collections {
		if time.Now().Before(c.until) {
			r.timer.Reset(ttl)
			return
		}
	}
	r.collections = make(map[string]*collection)
	r.texture = nil
	r.textureKey = ""
}

func (c *collection) index(ctx context.Context, mod bool) error {
	root, err := os.OpenRoot(c.root)
	if err != nil {
		return err
	}
	defer root.Close()
	budget := 16 << 20
	ambiguous := make(map[string]bool)
	add := func(name string, data []byte, bundled bool) error {
		budget -= len(data)
		if budget < 0 {
			return ErrLimit
		}
		atlas, err := maprenderer.ParseAtlas(data)
		if err != nil {
			return nil
		}
		texture := path.Join(path.Dir(name), atlas.Texture)
		if !fs.ValidPath(texture) || !strings.HasSuffix(texture, ".tex") {
			return nil
		}
		for key, element := range atlas.Elements {
			if len(c.icons) >= 10000 {
				return ErrLimit
			}
			if ambiguous[key] {
				continue
			}
			candidate := source{texture, bundled, element}
			if existing, exists := c.icons[key]; !exists {
				c.icons[key] = candidate
			} else if mod && existing != candidate {
				delete(c.icons, key)
				ambiguous[key] = true
			}
		}
		return nil
	}
	if !mod {
		bundle, err := openBundle(root)
		if err != nil {
			return err
		}
		defer bundle.file.Close()
		// Ignore the obsolete inventoryimages.xml when its texture is absent.
		names := make(map[string]bool, len(bundle.zip.File))
		for _, f := range bundle.zip.File {
			names[f.Name] = true
		}
		for _, f := range bundle.zip.File {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !inventoryAtlas.MatchString(f.Name) {
				continue
			}
			data, err := readZip(f, 2<<20)
			if err != nil {
				return err
			}
			atlas, err := maprenderer.ParseAtlas(data)
			if err != nil {
				continue
			}
			if !names[path.Join(path.Dir(f.Name), atlas.Texture)] {
				continue
			}
			if err := add(f.Name, data, true); err != nil {
				return err
			}
		}
		for _, name := range []string{"minimap/minimap_data.xml", "minimap/minimap_data1.xml", "minimap/minimap_data2.xml"} {
			data, err := readRoot(root, name, 2<<20)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if err := add(name, data, false); err != nil {
				return err
			}
		}
		return nil
	}
	// Bound traversal to the selected mod's image directories, never its Lua,
	// animation archives, another mod, or the save tree.
	entries, atlases := 0, 0
	for _, dir := range []string{"images", "minimap"} {
		err = walkImages(ctx, root, dir, 0, func(name string, d fs.DirEntry) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries++
			if entries > 8192 {
				return ErrLimit
			}
			if d.IsDir() || !strings.HasSuffix(name, ".xml") {
				return nil
			}
			atlases++
			if atlases > 512 {
				return ErrLimit
			}
			data, err := readRoot(root, name, 2<<20)
			if err != nil {
				return err
			}
			return add(name, data, false)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type bundleReader struct {
	file *os.File
	zip  *zip.Reader
}

func openBundle(root *os.Root) (bundleReader, error) {
	file, err := root.Open("databundles/images.zip")
	if err != nil {
		return bundleReader{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<20 {
		file.Close()
		return bundleReader{}, ErrLimit
	}
	z, err := zip.NewReader(file, info.Size())
	if err != nil {
		file.Close()
		return bundleReader{}, err
	}
	if len(z.File) > 10000 {
		file.Close()
		return bundleReader{}, ErrLimit
	}
	return bundleReader{file, z}, nil
}
func (c *collection) read(name string, bundled bool, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(c.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if !bundled {
		return readRoot(root, name, limit)
	}
	bundle, err := openBundle(root)
	if err != nil {
		return nil, err
	}
	defer bundle.file.Close()
	for _, f := range bundle.zip.File {
		if f.Name == name {
			return readZip(f, limit)
		}
	}
	return nil, fs.ErrNotExist
}
func readZip(file *zip.File, limit int64) ([]byte, error) {
	if file.UncompressedSize64 > uint64(limit) {
		return nil, ErrLimit
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return readLimited(reader, limit)
}
func readRoot(root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrLimit
	}
	return readLimited(file, limit)
}
func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: file size", ErrLimit)
	}
	return data, nil
}

// ValidatePNG checks untrusted Agent output before forwarding it to a browser.
func ValidatePNG(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxPNG {
		return ErrLimit
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if config.Width < 1 || config.Height < 1 || config.Width > 128 || config.Height > 128 {
		return ErrLimit
	}
	return nil
}

// Read directories in small chunks; a hostile mod cannot force WalkDir to
// allocate an unbounded directory listing before the entry budget is checked.
func walkImages(ctx context.Context, root *os.Root, directory string, depth int, visit func(string, fs.DirEntry) error) error {
	if depth > 16 {
		return ErrLimit
	}
	file, err := root.Open(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := file.ReadDir(64)
		for _, entry := range entries {
			name := path.Join(directory, entry.Name())
			if err := visit(name, entry); err != nil {
				return err
			}
			if entry.IsDir() {
				if err := walkImages(ctx, root, name, depth+1, visit); err != nil {
					return err
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
