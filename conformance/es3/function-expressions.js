function check(a,b){if(a!==b)throw a+" !== "+b;} var f=function(){return 42;};check(f(),42);check(typeof f,'function');console.log('PASS');
