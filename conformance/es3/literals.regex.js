function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof /abc/,'object');check(/abc/ instanceof RegExp,true);console.log('PASS');
