// Per-zone probe for zonedump.tsv: the long zone name ICU prints in the
// parenthesised tail of Date.prototype.toString(), sampled in each season.
var out = [];
function samp(ms) {
  var d = new Date(ms), s = d.toString(), i = s.lastIndexOf("(");
  return d.getTimezoneOffset() + "\t" + (i < 0 ? "" : s.slice(i + 1, -1));
}
out.push("JAN\t" + samp(1768435200000)); // 2026-01-15T00:00:00Z
out.push("JUL\t" + samp(1784073600000)); // 2026-07-15T00:00:00Z
