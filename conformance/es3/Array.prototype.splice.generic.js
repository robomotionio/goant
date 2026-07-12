function check(a,b){if(a!==b)throw a+" !== "+b;} var o={0:'a',1:'b',2:'c',length:3};var r=Array.prototype.splice.call(o,1,1);check(r[0],'b');check(o.length,2);console.log('PASS');
