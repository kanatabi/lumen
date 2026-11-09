package adminsvc

import "github.com/kanatabi/lumen/internal/store"

// The handlers' complete vocabulary: every wire DTO and sentinel the admin
// HTTP layer names is ALIASED here (type aliases — identical types, zero
// conversion), so internal/api/admin imports adminsvc + httpx and NOTHING
// below the service. The layering rule holds in imports, not just intent.

type (
	PostDraftInput   = store.PostDraftInput
	AdminListOpts    = store.AdminListOpts
	AdminPostSummary = store.AdminPostSummary
	AdminPostDetail  = store.AdminPostDetail
	PhotoListOpts    = store.PhotoListOpts
	PhotoPatch       = store.PhotoPatch
	AdminPhoto       = store.AdminPhoto
	PhotoDocRow      = store.PhotoDocRow
	PhotoRef         = store.PhotoRef
	TagRowOut        = store.TagRowOut
	TagPatch         = store.TagPatch
	CollectionRowOut = store.CollectionRowOut
)

var (
	ErrSlugTaken     = store.ErrSlugTaken
	ErrRevConflict   = store.ErrRevConflict
	ErrBadTransition = store.ErrBadTransition
	ErrNoPosts       = store.ErrNoPosts
	ErrNoPhoto       = store.ErrNoPhoto
	ErrUnknownColl   = store.ErrUnknownColl
	ErrDupVocab      = store.ErrDupVocab
	ErrBadName       = store.ErrBadName

	ErrUnknownVocab = store.ErrUnknownVocab
	ErrTagInUse     = store.ErrTagInUse
)
