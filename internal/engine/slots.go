package engine

// Internal object slots (ant common.h ANT_INTERNAL_SLOT_LIST). These are
// engine-private per-object fields stored outside the shape's named-property
// space (in the object's extra-slot sidecar). Only the engine-core slots are
// modeled here; ant's large host-module slot set (fs/http/worker/…) is out of
// scope. The concrete numeric values are internal to goant.
type internalSlot uint8

const (
	slotNone internalSlot = iota
	slotAsync
	slotCode
	slotCodeLen
	slotCFunc
	slotCoro
	slotProto
	slotFuncProto
	slotAsyncProto
	slotGeneratorProto
	slotAsyncGeneratorProto
	slotAux
	slotTargetFunc
	slotModuleCtx
	slotMap
	slotSet
	slotPrimitive
	slotProxyRef
	slotBrand
	slotPrivateElements
	slotData
	slotCtor
	slotDefault
	slotErrorBrand
	slotErrType
	slotStrictArgs
	slotIterState
	slotEntries
	slotSettled
	slotRegexpFlagsMask
	slotRegexpFlagsString
	slotRegexpNamedGroups
	slotRegexpResultGroups
	slotRegexpGroupsCache
	slotMatchallRx
	slotMatchallStr
	slotMatchallDone
	slotMax = 255
)

// brand ids for object internal-class checks (ant BRAND_*).
const brandNone = -1
