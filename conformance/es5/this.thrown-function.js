function check(a,b){if(a!==b)throw a+" !== "+b;} function f(){return this;}check(typeof f.call(null),'object');console.log('PASS');
