package domain

import (
	"time"

	"github.com/google/uuid"
)

type OpenEventOutcome string

const (
	OpenAllowed             OpenEventOutcome = "allowed"
	OpenDeniedWrongUser     OpenEventOutcome = "denied_wrong_user"
	OpenDeniedNotActive     OpenEventOutcome = "denied_not_active"
	OpenDeniedExpired       OpenEventOutcome = "denied_expired"
	OpenDeniedRevoked       OpenEventOutcome = "denied_revoked"
	OpenDeniedAlreadyOpened OpenEventOutcome = "denied_already_opened"
	OpenDeniedAmbiguous     OpenEventOutcome = "denied_ambiguous"
	OpenDeliveryFailed      OpenEventOutcome = "delivery_failed"
)

func (o OpenEventOutcome) IsValid() bool {
	switch o {
	case OpenAllowed, OpenDeniedWrongUser, OpenDeniedNotActive, OpenDeniedExpired, OpenDeniedRevoked, OpenDeniedAlreadyOpened, OpenDeniedAmbiguous, OpenDeliveryFailed:
		return true
	default:
		return false
	}
}

func (o OpenEventOutcome) Allowed() bool {
	return o == OpenAllowed
}

type WhisperOpenEvent struct {
	ID                int64
	WhisperID         uuid.UUID
	TelegramUserID    int64
	CallbackQueryID   string
	Outcome           OpenEventOutcome
	DeliveryState     OpenDeliveryState
	TelegramMessageID *int64
	DeliveryError     string
	CreatedAt         time.Time
	CompletedAt       *time.Time
}

type OpenDeliveryState string

const (
	OpenDeliveryNotAttempted OpenDeliveryState = "not_attempted"
	OpenDeliveryReserved     OpenDeliveryState = "reserved"
	OpenDeliveryDelivered    OpenDeliveryState = "delivered"
	OpenDeliveryFailure      OpenDeliveryState = "failed"
)

func (s OpenDeliveryState) IsValid() bool {
	switch s {
	case OpenDeliveryNotAttempted, OpenDeliveryReserved, OpenDeliveryDelivered, OpenDeliveryFailure:
		return true
	default:
		return false
	}
}

// OwnerAuditAction identifies sensitive operator actions. Every media read or
// destructive/retention change should produce one immutable audit event.
type OwnerAuditAction string

const (
	OwnerAuditViewMetadata    OwnerAuditAction = "view_metadata"
	OwnerAuditRetrieveContent OwnerAuditAction = "retrieve_content"
	OwnerAuditRetrieveMedia   OwnerAuditAction = "retrieve_media"
	OwnerAuditDeleteMedia     OwnerAuditAction = "delete_media"
	OwnerAuditDeleteWhisper   OwnerAuditAction = "delete_whisper"
	OwnerAuditUpdateRetention OwnerAuditAction = "update_retention"
	OwnerAuditRevokeWhisper   OwnerAuditAction = "revoke_whisper"
)

func (a OwnerAuditAction) IsValid() bool {
	switch a {
	case OwnerAuditViewMetadata, OwnerAuditRetrieveContent, OwnerAuditRetrieveMedia, OwnerAuditDeleteMedia, OwnerAuditDeleteWhisper, OwnerAuditUpdateRetention, OwnerAuditRevokeWhisper:
		return true
	default:
		return false
	}
}

type OwnerAuditEvent struct {
	ID                  int64
	OwnerTelegramUserID int64
	WhisperID           *uuid.UUID
	Action              OwnerAuditAction
	Success             bool
	Reason              string
	Details             map[string]string
	CreatedAt           time.Time
}
