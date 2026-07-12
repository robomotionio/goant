function check(a,b){if(a!==b)throw a+" !== "+b;} check((function(a,b){return a+b;}).apply(null,[2,3]),5);console.log('PASS');
