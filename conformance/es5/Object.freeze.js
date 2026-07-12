function check(a,b){if(a!==b)throw a+" !== "+b;} var o={a:1};Object.freeze(o);o.a=2;check(o.a,1);o.b=3;check(o.b,undefined);check(Object.isFrozen(o),true);console.log('PASS');
