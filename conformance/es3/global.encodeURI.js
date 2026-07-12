function check(a,b){if(a!==b)throw a+" !== "+b;} check(encodeURI('http://x/a b'),'http://x/a%20b');console.log('PASS');
