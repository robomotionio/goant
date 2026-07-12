function check(a,b){if(a!==b)throw a+" !== "+b;} check('  hi  '.trim(),'hi');check('\tx\n'.trim(),'x');check('abc'.trim(),'abc');console.log('PASS');
