package worldmap

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"os"
	"path/filepath"
)

// MemoryRenderer is a deterministic renderer wired only by the router in test mode.
type MemoryRenderer struct{}

func NewMemoryRenderer() *MemoryRenderer { return &MemoryRenderer{} }

func (*MemoryRenderer) Available() (bool, string) { return true, "memory" }

func (*MemoryRenderer) Render(ctx context.Context, input, output string, layers []Layer, log io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(input); err != nil {
		return err
	}
	for _, layer := range layers {
		canvas := image.NewRGBA(image.Rect(0, 0, 960, 640))
		if layer == LayerTerrain {
			draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.RGBA{R: 29, G: 74, B: 48, A: 255}}, image.Point{}, draw.Src)
			for y := 0; y < 640; y += 80 {
				shade := color.RGBA{R: uint8(48 + y/20), G: uint8(105 + y/24), B: 61, A: 255}
				draw.Draw(canvas, image.Rect(80, y, 880, y+48), &image.Uniform{C: shade}, image.Point{}, draw.Src)
			}
		} else {
			draw.Draw(canvas, canvas.Bounds(), image.Transparent, image.Point{}, draw.Src)
			marker := map[Layer]color.RGBA{
				LayerWalrusCamps: {R: 238, G: 131, B: 55, A: 230}, LayerSpawnPoints: {R: 255, G: 219, B: 88, A: 230},
				LayerPlayers: {R: 244, G: 91, B: 105, A: 230}, LayerWorldState: {R: 107, G: 205, B: 127, A: 230},
			}[layer]
			draw.Draw(canvas, image.Rect(420, 260, 540, 380), &image.Uniform{C: marker}, image.Point{}, draw.Src)
		}
		file, err := os.OpenFile(filepath.Join(output, layerFileName(layer)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if err != nil {
			return err
		}
		encodeErr := png.Encode(file, canvas)
		closeErr := file.Close()
		if encodeErr != nil {
			return encodeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	_, err := fmt.Fprintf(log, "[DST-ADMIN-TEST] rendered %d map layers\n", len(layers))
	return err
}
