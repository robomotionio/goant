function check(a,b){if(a!==b)throw a+" !== "+b;} var o={1:'a',2:'b'};check(o[1],'a');check(o['2'],'b');console.log('PASS');
