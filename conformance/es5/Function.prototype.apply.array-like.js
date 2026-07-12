function check(a,b){if(a!==b)throw a+" !== "+b;} function f(){return arguments.length;}check(f.apply(null,{length:3,0:1,1:2,2:3}),3);check(f.apply(null,[1,2]),2);console.log('PASS');
