function check(a,b){if(a!==b)throw a+" !== "+b;} var o={a:1};Object.seal(o);o.b=2;check(o.b,undefined);o.a=5;check(o.a,5);check(Object.isSealed(o),true);console.log('PASS');
