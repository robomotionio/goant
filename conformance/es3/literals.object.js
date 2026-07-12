function check(a,b){if(a!==b)throw a+" !== "+b;} var o={a:1,b:2};check(o.a,1);check(o.b,2);var e={};check(typeof e,'object');console.log('PASS');
