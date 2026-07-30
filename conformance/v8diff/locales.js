var out=[];
var LOC=["af-ZA", "am-ET", "az-AZ", "bg-BG", "bs-BA", "ca-ES", "cs-CZ", "da-DK", "de-AT", "de-CH", "de-DE", "el-GR", "en-AU", "en-CA", "en-GB", "en-IE", "en-IN", "en-NZ", "en-PH", "en-SG", "en-US", "en-ZA", "es-AR", "es-CL", "es-CO", "es-ES", "es-MX", "es-PE", "es-VE", "et-EE", "fi-FI", "fil-PH", "fr-BE", "fr-CA", "fr-CH", "fr-FR", "gu-IN", "he-IL", "hi-IN", "hr-HR", "hu-HU", "id-ID", "it-CH", "it-IT", "ja-JP", "kn-IN", "ko-KR", "lt-LT", "lv-LV", "ml-IN", "ms-MY", "nb-NO", "nl-BE", "nl-NL", "pa-IN", "pl-PL", "pt-BR", "pt-PT", "ro-RO", "ru-RU", "sk-SK", "sl-SI", "sr-RS", "sv-SE", "sw-KE", "sw-TZ", "ta-IN", "te-IN", "tr-TR", "uk-UA", "ur-PK", "uz-UZ", "vi-VN", "zh-CN", "zh-HK", "zh-TW", "en", "de", "fr", "tr", "pt", "zh", "es", "EN-us", "en-US-u-ca-gregory", "xx-YY", "de-LU", "en-XX"];

var D=[Date.UTC(2026,0,2,9,4,5),Date.UTC(2026,10,22,15,7,9),Date.UTC(2026,5,1,0,0,0),
       Date.UTC(2026,5,1,12,0,0),Date.UTC(2026,5,1,23,59,59),0,-86400000];
for(var i=0;i<LOC.length;i++){for(var j=0;j<D.length;j++){
  var d=new Date(D[j]);
  out.push(LOC[i]+"|"+j+"|"+d.toLocaleDateString(LOC[i])+"|"+d.toLocaleTimeString(LOC[i])+"|"+d.toLocaleString(LOC[i]));
}}
var N=[1.23456,1.2345,0.5,1234.5678,1e21,1e-7,0.0001,-0,0,NaN,Infinity,-Infinity,
       1234567890123456789,123456.789,-1234.5,1/3,2.5,999.9999,1234,12345,123456789,
       -0.0004,9999.9995,1e-3,0.125];
for(var i=0;i<LOC.length;i++){for(var j=0;j<N.length;j++){
  out.push(LOC[i]+"|n"+j+"|"+N[j].toLocaleString(LOC[i]));
}}

