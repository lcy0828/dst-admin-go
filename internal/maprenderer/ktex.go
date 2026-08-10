package maprenderer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"

	"github.com/woozymasta/bcn"
)

const (
	maxTextureDimension = 16384
	maxTextureMipmaps   = 24
)

func DecodeKTEX(data []byte) (*image.NRGBA, error) {
	if len(data) < 18 || string(data[:4]) != "KTEX" {
		return nil, errors.New("invalid or truncated KTEX texture")
	}
	bits := binary.LittleEndian.Uint32(data[4:8])
	compression := int((bits >> 4) & 0x1f)
	mipmapCount := int((bits >> 13) & 0x1f)
	if mipmapCount < 1 || mipmapCount > maxTextureMipmaps {
		return nil, fmt.Errorf("invalid KTEX mipmap count %d", mipmapCount)
	}
	metadataEnd := 8 + mipmapCount*10
	if metadataEnd > len(data) {
		return nil, errors.New("KTEX mipmap metadata is truncated")
	}
	width := int(binary.LittleEndian.Uint16(data[8:10]))
	height := int(binary.LittleEndian.Uint16(data[10:12]))
	dataSize := int(binary.LittleEndian.Uint32(data[14:18]))
	if width < 1 || height < 1 || width > maxTextureDimension || height > maxTextureDimension || dataSize < 1 || metadataEnd+dataSize > len(data) {
		return nil, errors.New("KTEX first mipmap is invalid")
	}
	payload := data[metadataEnd : metadataEnd+dataSize]
	var format bcn.Format
	switch compression {
	case 0:
		format = bcn.FormatBC1
	case 1:
		format = bcn.FormatBC2
	case 2:
		format = bcn.FormatBC3
	case 4:
		format = bcn.FormatRGBA8
	case 5:
		return decodeRGBTexture(payload, width, height)
	default:
		return nil, fmt.Errorf("unsupported KTEX compression %d", compression)
	}
	decoded, err := bcn.DecodeImage(payload, width, height, format)
	if err != nil {
		return nil, fmt.Errorf("decode KTEX %s texture: %w", format.String(), err)
	}
	return decoded, nil
}

func decodeRGBTexture(data []byte, width, height int) (*image.NRGBA, error) {
	if len(data) != width*height*3 {
		return nil, errors.New("uncompressed RGB KTEX has an invalid payload size")
	}
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	for source, target := 0, 0; source < len(data); source, target = source+3, target+4 {
		copy(result.Pix[target:target+3], data[source:source+3])
		result.Pix[target+3] = 255
	}
	return result, nil
}
