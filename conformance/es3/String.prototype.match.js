function check(a,b){if(a!==b)throw a+" !== "+b;} check('hello world'.match(/o/g).length,2);check('abc'.match(/(\w)(\w)/)[2],'b');console.log('PASS');
