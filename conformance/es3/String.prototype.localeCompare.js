function check(a,b){if(a!==b)throw a+" !== "+b;} check('a'.localeCompare('b')<0,true);check('b'.localeCompare('a')>0,true);check('a'.localeCompare('a'),0);console.log('PASS');
