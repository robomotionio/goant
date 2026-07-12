function check(a,b){if(a!==b)throw a+" !== "+b;} var o=JSON.parse('{"a":1,"b":2}',function(k,v){return typeof v==='number'?v*2:v;});check(o.a,2);check(o.b,4);console.log('PASS');
