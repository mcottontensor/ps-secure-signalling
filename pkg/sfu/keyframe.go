package sfu

import (
	"strings"

	"github.com/pion/webrtc/v4"
)

// keyframeFunc reports whether an RTP payload is the first packet of a keyframe, which is the only
// safe place to switch a player from one simulcast layer to another.
type keyframeFunc func(payload []byte) bool

func keyframeDetector(mime string) keyframeFunc {
	switch strings.ToLower(mime) {
	case strings.ToLower(webrtc.MimeTypeVP8):
		return isVP8Keyframe
	case strings.ToLower(webrtc.MimeTypeVP9):
		return isVP9Keyframe
	case strings.ToLower(webrtc.MimeTypeH264):
		return isH264Keyframe
	case strings.ToLower(webrtc.MimeTypeAV1):
		return isAV1Keyframe
	}
	// Audio, or a codec we cannot inspect: every packet is a valid starting point.
	return func([]byte) bool { return true }
}

// isVP8Keyframe parses the VP8 payload descriptor (RFC 7741 section 4.2) and checks the inverse
// key frame bit of the VP8 payload header that follows it.
func isVP8Keyframe(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	start, pid := p[0]&0x10 != 0, p[0]&0x07
	if !start || pid != 0 {
		return false
	}
	i := 1
	if p[0]&0x80 != 0 { // X: extension byte
		if len(p) <= i {
			return false
		}
		x := p[i]
		i++
		if x&0x80 != 0 { // I: picture id, one or two bytes
			if len(p) <= i {
				return false
			}
			if p[i]&0x80 != 0 {
				i++
			}
			i++
		}
		if x&0x40 != 0 { // L: TL0PICIDX
			i++
		}
		if x&0x30 != 0 { // T or K: TID/KEYIDX
			i++
		}
	}
	return len(p) > i && p[i]&0x01 == 0
}

// isVP9Keyframe checks the VP9 payload descriptor: start of a frame, not inter-picture predicted,
// and the base spatial layer.
func isVP9Keyframe(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	d := p[0]
	interPredicted, layers, begin := d&0x40 != 0, d&0x20 != 0, d&0x08 != 0
	if !begin || interPredicted {
		return false
	}
	if !layers {
		return true
	}
	i := 1
	if d&0x80 != 0 { // I: picture id
		if len(p) <= i {
			return false
		}
		if p[i]&0x80 != 0 {
			i++
		}
		i++
	}
	return len(p) > i && (p[i]>>1)&0x07 == 0 // SID
}

// isH264Keyframe looks for an SPS or IDR NAL unit in a single NAL, STAP-A or FU-A start packet.
func isH264Keyframe(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	isKey := func(t byte) bool { return t == 5 || t == 7 }
	switch t := p[0] & 0x1f; {
	case t >= 1 && t <= 23:
		return isKey(t)
	case t == 24: // STAP-A: [hdr][size16][nal]...
		for i := 1; i+2 < len(p); {
			size := int(p[i])<<8 | int(p[i+1])
			i += 2
			if size == 0 || i+size > len(p) {
				return false
			}
			if isKey(p[i] & 0x1f) {
				return true
			}
			i += size
		}
	case t == 28: // FU-A
		return len(p) > 1 && p[1]&0x80 != 0 && isKey(p[1]&0x1f)
	}
	return false
}

// isAV1Keyframe uses the aggregation header's N bit (first packet of a new coded video sequence),
// which AV1 encoders set on keyframes.
func isAV1Keyframe(p []byte) bool {
	return len(p) > 0 && p[0]&0x80 == 0 && p[0]&0x08 != 0
}
