function check(a,b){if(a!==b)throw a+" !== "+b;} check('foo123'.match(/(\w+?)(\d+)/)[2],'123');console.log('PASS');
