function check(a,b){if(a!==b)throw a+" !== "+b;} var o={};Object.defineProperties(o,{a:{value:1,enumerable:true},b:{value:2,enumerable:true}});check(o.a,1);check(o.b,2);console.log('PASS');
