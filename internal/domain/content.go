package domain

import (
	"errors"

	"github.com/google/uuid"
)

type PayloadKind string

const (
	PayloadText  PayloadKind = "text"
	PayloadMedia PayloadKind = "media"
)

func (k PayloadKind) IsValid() bool {
	return k == PayloadText || k == PayloadMedia
}

var (
	ErrInvalidPayloadKind    = errors.New("invalid payload kind")
	ErrInvalidTextBlobID     = errors.New("text payload requires a blob ID")
	ErrInvalidCaptionBlobID  = errors.New("caption blob ID must be non-zero")
	ErrMissingMediaReference = errors.New("media payload requires a media reference")
	ErrUnexpectedTextBlob    = errors.New("media payload cannot contain a text blob")
	ErrUnexpectedMedia       = errors.New("text payload cannot contain media")
	ErrUnexpectedCaption     = errors.New("text payload cannot contain a caption")
)

// ContentReference contains storage identifiers only. The referenced text,
// caption and media blobs are encrypted by the persistence layer; plaintext
// and ciphertext bytes never live on Whisper itself.
type ContentReference struct {
	Kind          PayloadKind
	TextBlobID    *uuid.UUID
	Media         *MediaReference
	CaptionBlobID *uuid.UUID
}

func (c ContentReference) Validate() error {
	if !c.Kind.IsValid() {
		return ErrInvalidPayloadKind
	}

	switch c.Kind {
	case PayloadText:
		if c.TextBlobID == nil || *c.TextBlobID == uuid.Nil {
			return ErrInvalidTextBlobID
		}
		if c.Media != nil {
			return ErrUnexpectedMedia
		}
		if c.CaptionBlobID != nil {
			return ErrUnexpectedCaption
		}
	case PayloadMedia:
		if c.TextBlobID != nil {
			return ErrUnexpectedTextBlob
		}
		if c.Media == nil {
			return ErrMissingMediaReference
		}
		if err := c.Media.Validate(); err != nil {
			return err
		}
		if c.CaptionBlobID != nil && *c.CaptionBlobID == uuid.Nil {
			return ErrInvalidCaptionBlobID
		}
	}

	return nil
}

func cloneContentReference(content ContentReference) ContentReference {
	clone := ContentReference{
		Kind:          content.Kind,
		TextBlobID:    cloneUUID(content.TextBlobID),
		CaptionBlobID: cloneUUID(content.CaptionBlobID),
	}
	if content.Media != nil {
		media := *content.Media
		media.BlobID = cloneUUID(content.Media.BlobID)
		clone.Media = &media
	}
	return clone
}

func cloneUUID(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
