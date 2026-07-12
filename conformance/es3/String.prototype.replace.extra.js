function check(a,b){if(a!==b)throw a+" !== "+b;} check('abc'.replace(/b/,'[$&]'),'a[b]c');console.log('PASS');
