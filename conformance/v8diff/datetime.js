function t(label, fn) {
  var v;
  try { v = String(fn()); } catch (e) { v = "THROW: " + e.message; }
  var pad = label; while (pad.length < 32) pad += " ";
  out.push(pad + "| " + v);
}
var out = [];
var FIXED = new Date(1767225600000);  // 2026-01-01T00:00:00Z
var SUMMER = new Date(1782000000000); // 2026-06-21T04:00:00Z, the other side of DST

t("typeof Intl", function(){ return typeof Intl });
t("getTimezoneOffset() winter", function(){ return FIXED.getTimezoneOffset() });
t("getTimezoneOffset() summer", function(){ return SUMMER.getTimezoneOffset() });
t("FIXED.toString()", function(){ return FIXED.toString() });
t("FIXED.toDateString()", function(){ return FIXED.toDateString() });
t("FIXED.toTimeString()", function(){ return FIXED.toTimeString() });
t("FIXED.toUTCString()", function(){ return FIXED.toUTCString() });
t("FIXED.toISOString()", function(){ return FIXED.toISOString() });
t("FIXED.toLocaleDateString()", function(){ return FIXED.toLocaleDateString() });
t("FIXED.toLocaleTimeString()", function(){ return FIXED.toLocaleTimeString() });
t("FIXED.toLocaleString()", function(){ return FIXED.toLocaleString() });
t("FIXED.getFullYear()", function(){ return FIXED.getFullYear() });
t("FIXED.getMonth()", function(){ return FIXED.getMonth() });
t("FIXED.getDate()", function(){ return FIXED.getDate() });
t("FIXED.getDay()", function(){ return FIXED.getDay() });
t("FIXED.getHours()", function(){ return FIXED.getHours() });
t("FIXED.getUTCHours()", function(){ return FIXED.getUTCHours() });
t("new Date(2026,6,30).toISOString", function(){ return new Date(2026,6,30).toISOString() });
t("new Date(2026,6,30,9).toISOString", function(){ return new Date(2026,6,30,9).toISOString() });
t("Date.UTC(2026,6,30)", function(){ return Date.UTC(2026,6,30) });
t("parse '2026-07-30'", function(){ return new Date(Date.parse('2026-07-30')).toISOString() });
t("parse '2026-07-30T09:00'", function(){ return new Date(Date.parse('2026-07-30T09:00')).toISOString() });
t("parse '2026-07-30T09:00Z'", function(){ return new Date(Date.parse('2026-07-30T09:00Z')).toISOString() });
t("parse '2026/07/30'", function(){ return new Date(Date.parse('2026/07/30')).toISOString() });
t("parse 'Jul 30, 2026'", function(){ return new Date(Date.parse('Jul 30, 2026')).toISOString() });
t("parse 'July 30, 2026'", function(){ return new Date(Date.parse('July 30, 2026')).toISOString() });
t("parse '30 Jul 2026'", function(){ return new Date(Date.parse('30 Jul 2026')).toISOString() });
t("parse(FIXED.toString())", function(){ return new Date(Date.parse(FIXED.toString())).toISOString() });
t("setDate roundtrip (rollover)", function(){ var d=new Date(1767225600000); d.setDate(d.getDate()); return d.toISOString() });
t("(1234.5).toLocaleString()", function(){ return (1234.5).toLocaleString() });

// The customer's script, verbatim in behaviour
var mS=['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];
function cust(base) {
  var date=new Date(base); date.setDate(date.getDate());
  var str=date.toLocaleDateString();
  var day=str.split(" ")[2], month=str.split(" ")[1], year=str.split(" ")[3];
  var mn=0; for(var i=0;i<mS.length;i++){if(mS[i]==month){mn=i+1}}
  var m2=mn<10?'0'+mn:''+mn;
  return { str: str, todayDate: day+"."+m2+"."+year, file_format: year+"-"+m2+"-"+day };
}
t("CUSTOMER str (2026-01-01T00Z)", function(){ return cust(1767225600000).str });
t("CUSTOMER todayDate", function(){ return cust(1767225600000).todayDate });
t("CUSTOMER file_format", function(){ return cust(1767225600000).file_format });
