function check(a,b){if(a!==b)throw a+" !== "+b;} check(Object.isFrozen({}),false);check(Object.isFrozen(Object.freeze({})),true);console.log('PASS');
