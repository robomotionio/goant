function check(a,b){if(a!==b)throw a+" !== "+b;} var o={a:1};check(o.hasOwnProperty('a'),true);check(o.hasOwnProperty('toString'),false);console.log('PASS');
