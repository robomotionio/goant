function check(a,b){if(a!==b)throw a+" !== "+b;} check(Object.keys({a:1,b:2,c:3}).join(','),'a,b,c');check(Object.keys({}).length,0);console.log('PASS');
