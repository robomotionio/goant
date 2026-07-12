function check(a,b){if(a!==b)throw a+" !== "+b;} check('2023-01'.replace(/-/,'/'),'2023/01');check('aaa'.replace(/a/g,'b'),'bbb');console.log('PASS');
