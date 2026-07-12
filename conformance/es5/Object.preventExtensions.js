function check(a,b){if(a!==b)throw a+" !== "+b;} var o={};Object.preventExtensions(o);o.x=1;check(o.x,undefined);check(Object.isExtensible(o),false);console.log('PASS');
