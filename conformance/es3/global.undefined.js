function check(a,b){if(a!==b)throw a+" !== "+b;} check(undefined,undefined);check(typeof undefined,'undefined');check(void 0,undefined);console.log('PASS');
