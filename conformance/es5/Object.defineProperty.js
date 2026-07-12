function check(a,b){if(a!==b)throw a+" !== "+b;} var o={};Object.defineProperty(o,'x',{value:42,enumerable:true});check(o.x,42);check(Object.keys(o).length,1);console.log('PASS');
