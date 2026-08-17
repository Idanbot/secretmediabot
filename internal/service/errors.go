// Package service implements the bot's privacy and lifecycle use cases.
package service

import "errors"

var (
	ErrTargetRequired       = errors.New("a whisper target is required")
	ErrInvalidMembership    = errors.New("invalid Telegram group membership")
	ErrTargetNotObserved    = errors.New("target has not been observed in this group")
	ErrAmbiguousTarget      = errors.New("target username is ambiguous")
	ErrTargetIsBot          = errors.New("target is a bot")
	ErrTargetIsSender       = errors.New("sender and target must differ")
	ErrChatNotAllowed       = errors.New("chat is not allowed")
	ErrTooManyDrafts        = errors.New("too many active drafts")
	ErrRateLimited          = errors.New("whisper rate limit exceeded")
	ErrDraftNotFound        = errors.New("active draft not found")
	ErrDraftExpired         = errors.New("draft expired")
	ErrDraftBusy            = errors.New("draft is already ingesting content")
	ErrUnsupportedContent   = errors.New("content must be text or one supported media item")
	ErrContentTooLarge      = errors.New("content exceeds the configured size limit")
	ErrTextTooLong          = errors.New("secret text exceeds Telegram's limit")
	ErrCaptionTooLong       = errors.New("secret caption exceeds Telegram's limit")
	ErrInvalidOpenToken     = errors.New("invalid whisper token")
	ErrWhisperNotFound      = errors.New("whisper not found")
	ErrWrongRecipient       = errors.New("whisper belongs to another recipient")
	ErrWhisperExpired       = errors.New("whisper expired")
	ErrWhisperRevoked       = errors.New("whisper revoked")
	ErrWhisperAlreadyOpened = errors.New("whisper already opened")
	ErrWhisperUnavailable   = errors.New("whisper is not available")
	ErrOwnerOnly            = errors.New("owner authorization required")
)
