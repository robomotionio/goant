// Package regexpjs is the JS→regex translation layer: it parses/validates
// ECMAScript regular expressions and retargets them onto dlclark/regexp2,
// with translate-time expansion of property escapes (UCD 17.0) and v-flag set
// notation, sticky/anchored program variants, and ported RegExp.$1-$9 statics.
// See PLAN.md Phase 4.3 / 8.
package regexpjs
