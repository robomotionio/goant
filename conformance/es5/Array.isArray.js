function check(a,b){if(a!==b)throw a+" !== "+b;} check(Array.isArray([]),true);check(Array.isArray({}),false);check(Array.isArray([1,2]),true);check(Array.isArray('abc'),false);console.log('PASS');
