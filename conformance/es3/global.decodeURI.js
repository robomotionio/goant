function check(a,b){if(a!==b)throw a+" !== "+b;} check(decodeURI('a%20b'),'a b');console.log('PASS');
