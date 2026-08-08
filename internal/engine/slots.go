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
	slotBoundThis
	slotBoundArgs
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
	slotRawJSON           // a JSON.rawJSON object's [[RawJSON]] text (a String)
	slotImmutableBuffer   // marks an ArrayBuffer's [[ArrayBufferIsImmutable]] slot
	slotWeakRefTarget     // a WeakRef's [[WeakRefTarget]] (also its brand)
	slotErrorFrames       // an error's captured call sites (an Array of CallSite)
	slotCallSite          // one CallSite's fields (an Array; see builtin_error_stack.go)
	slotIntlLocale        // an Intl service's resolved locale tag (a String)
	slotIntlTimeZone      // a DateTimeFormat's [[TimeZone]] identifier (a String)
	slotLocaleTag         // an Intl.Locale's [[Locale]] tag (a String), also its brand
	slotIntlPluralOpts    // a PluralRules' type and digit options (a String; see builtin_intl_plural.go)
	slotIntlCollatorOpts  // a Collator's resolved options (a String; see builtin_intl_collator.go)
	slotIntlNumberOpts    // a NumberFormat's resolved options (a String; see builtin_intl_number.go)
	slotIntlListOpts      // a ListFormat's resolved options (a String; see builtin_intl_list.go)
	slotIntlRelTimeOpts   // a RelativeTimeFormat's resolved options (a String)
	slotIntlDisplayOpts   // a DisplayNames' resolved options (a String)
	slotIntlSegmenterOpts // a Segmenter's resolved options (a String)
	slotSegmentsInput     // a Segments object's string
	slotSegmentsOpts      // a Segments/Segment-iterator's segmenter options
	slotSegIterPos        // a Segment iterator's position, in code units
	slotMax               = 255
)

// brand ids for object internal-class checks (ant BRAND_*).
const brandNone = -1
