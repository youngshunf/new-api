package ali

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// normalizeAliWAV只处理已完整、有界收齐的响应；不能把任意截断WAV当作流式长度修复。
func normalizeAliWAV(data []byte) ([]byte, error) {
	invalid := errors.New("invalid Ali PCM WAV structure or length")
	if len(data) < 44 || len(data) > 64*1024*1024 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, invalid
	}
	riffSize := binary.LittleEndian.Uint32(data[4:8])
	// 此组合来自真实CosyVoice响应；只接受标准44字节PCM16/mono/24k头及完整帧。
	if riffSize == 0x7fffffbf && string(data[12:16]) == "fmt " && binary.LittleEndian.Uint32(data[16:20]) == 16 &&
		string(data[36:40]) == "data" && binary.LittleEndian.Uint32(data[40:44]) == 0x7fffff9b {
		format := data[20:36]
		if !validAliPCMFormat(format) || binary.LittleEndian.Uint16(format[2:4]) != 1 || binary.LittleEndian.Uint32(format[4:8]) != 24000 ||
			binary.LittleEndian.Uint16(format[14:16]) != 16 || len(data) == 44 || (len(data)-44)%2 != 0 {
			return nil, invalid
		}
		result := bytes.Clone(data)
		binary.LittleEndian.PutUint32(result[4:8], uint32(len(result)-8))
		binary.LittleEndian.PutUint32(result[40:44], uint32(len(result)-44))
		return result, nil
	}
	if uint64(riffSize) != uint64(len(data)-8) {
		return nil, invalid
	}
	var format []byte
	dataChunks := 0
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return nil, invalid
		}
		name := string(data[offset : offset+4])
		size := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		if size > uint64(len(data)-start) {
			return nil, invalid
		}
		end := start + int(size)
		switch name {
		case "fmt ":
			if format != nil || (size != 16 && size != 18) || (size == 18 && binary.LittleEndian.Uint16(data[start+16:end]) != 0) {
				return nil, invalid
			}
			format = data[start : start+16]
			if !validAliPCMFormat(format) {
				return nil, invalid
			}
		case "data":
			dataChunks++
			if format == nil || dataChunks != 1 || size == 0 || size%uint64(binary.LittleEndian.Uint16(format[12:14])) != 0 {
				return nil, invalid
			}
		}
		offset = end + int(size%2)
		if offset > len(data) {
			return nil, invalid
		}
	}
	if format == nil || dataChunks != 1 {
		return nil, invalid
	}
	return data, nil
}

func validAliPCMFormat(format []byte) bool {
	if len(format) != 16 || binary.LittleEndian.Uint16(format[:2]) != 1 {
		return false
	}
	channels := uint64(binary.LittleEndian.Uint16(format[2:4]))
	sampleRate := uint64(binary.LittleEndian.Uint32(format[4:8]))
	byteRate := uint64(binary.LittleEndian.Uint32(format[8:12]))
	blockAlign := uint64(binary.LittleEndian.Uint16(format[12:14]))
	bits := uint64(binary.LittleEndian.Uint16(format[14:16]))
	return channels > 0 && sampleRate > 0 && (bits == 8 || bits == 16 || bits == 24 || bits == 32) &&
		blockAlign == channels*(bits/8) && byteRate == sampleRate*blockAlign
}
