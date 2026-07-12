function check(a,b){if(a!==b)throw a+" !== "+b;} check(Object.isSealed({}),false);check(Object.isSealed(Object.seal({})),true);console.log('PASS');
