function check(a,b){if(a!==b)throw a+" !== "+b;} check(encodeURIComponent('a b&c'),'a%20b%26c');console.log('PASS');
