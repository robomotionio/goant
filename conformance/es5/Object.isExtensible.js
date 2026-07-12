function check(a,b){if(a!==b)throw a+" !== "+b;} check(Object.isExtensible({}),true);var o={};Object.preventExtensions(o);check(Object.isExtensible(o),false);console.log('PASS');
