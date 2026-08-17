package domain

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

type MediaType string

const (
	MediaPhoto    MediaType = "photo"
	MediaVoice    MediaType = "voice"
	MediaVideo    MediaType = "video"
	MediaAudio    MediaType = "audio"
	MediaDocument MediaType = "document"
)

func (t MediaType) IsValid() bool {
	switch t {
	case MediaPhoto, MediaVoice, MediaVideo, MediaAudio, MediaDocument:
		return true
	default:
		return false
	}
}

type MediaProvider string

const (
	// MediaProviderTelegram means Ref is a reusable Telegram file_id and
	// UniqueRef, when set, is Telegram's file_unique_id.
	MediaProviderTelegram MediaProvider = "telegram"
	// MediaProviderPostgresBlob means BlobID references a separately stored
	// database blob. Media bytes deliberately do not live on Whisper.
	MediaProviderPostgresBlob MediaProvider = "postgres_blob"
)

func (p MediaProvider) IsValid() bool {
	return p == MediaProviderTelegram || p == MediaProviderPostgresBlob
}

var (
	ErrInvalidMediaProvider = errors.New("invalid media provider")
	ErrInvalidMediaType     = errors.New("invalid media type")
	ErrEmptyMediaReference  = errors.New("empty media reference")
	ErrInvalidMediaBlobID   = errors.New("postgres media reference requires a blob ID")
	ErrInvalidMediaSize     = errors.New("media size cannot be negative")
)

// MediaReference identifies media without storing its bytes in this service.
// Ref and UniqueRef must never be written to ordinary application logs.
type MediaReference struct {
	Provider    MediaProvider
	Type        MediaType
	Ref         string
	UniqueRef   string
	BlobID      *uuid.UUID
	ContentType string
	SizeBytes   int64
}

func (m MediaReference) Validate() error {
	if !m.Provider.IsValid() {
		return ErrInvalidMediaProvider
	}
	if !m.Type.IsValid() {
		return ErrInvalidMediaType
	}
	if m.SizeBytes < 0 {
		return ErrInvalidMediaSize
	}
	switch m.Provider {
	case MediaProviderTelegram:
		if strings.TrimSpace(m.Ref) == "" {
			return ErrEmptyMediaReference
		}
	case MediaProviderPostgresBlob:
		if m.BlobID == nil || *m.BlobID == uuid.Nil {
			return ErrInvalidMediaBlobID
		}
		if strings.TrimSpace(m.Ref) == "" {
			return ErrEmptyMediaReference
		}
	default:
		return ErrEmptyMediaReference
	}
	return nil
}
